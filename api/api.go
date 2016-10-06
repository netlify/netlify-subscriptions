package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/dgrijalva/jwt-go"
	"github.com/guregu/kami"
	"github.com/pborman/uuid"
	"github.com/rs/cors"

	"github.com/jinzhu/gorm"
	"github.com/netlify/netlify-subscriptions/conf"
	"github.com/zenazn/goji/web/mutil"
)

type API struct {
	log        *logrus.Entry
	config     *conf.Config
	port       int64
	handler    http.Handler
	db         *gorm.DB
	payerProxy payerProxy
}

type JWTClaims struct {
	jwt.StandardClaims
	ID     string
	Email  string
	Groups []string
}

var bearerRegexp = regexp.MustCompile(`^(?:B|b)earer (\S+$)`)

func NewAPI(config *conf.Config, db *gorm.DB, proxy payerProxy) *API {
	api := &API{
		log:        logrus.WithField("component", "api"),
		config:     config,
		port:       config.Port,
		db:         db,
		payerProxy: proxy,
	}

	k := kami.New()
	k.LogHandler = logCompleted

	k.Use("/", api.populateConfig)

	k.Get("/", hello)
	k.Get("/subscriptions", listSubs)
	k.Get("/subscriptions/:type", viewSub)
	k.Put("/subscriptions/:type", createOrModSub)
	k.Delete("/subscriptions/:type", deleteSub)

	corsHandler := cors.New(cors.Options{
		AllowedMethods:   []string{"GET", "POST", "PATCH", "PUT", "DELETE"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
		AllowCredentials: true,
	})

	api.handler = corsHandler.Handler(k)
	return api
}

func (a *API) Serve() error {
	return http.ListenAndServe(fmt.Sprintf(":%d", a.port), a.handler)
}

func logCompleted(ctx context.Context, wp mutil.WriterProxy, r *http.Request) {
	log := getLogger(ctx).WithField("status", wp.Status())

	start := getStartTime(ctx)
	if start != nil {
		log = log.WithField("duration", time.Since(*start))
	}

	log.Infof("Completed request %s. path: %s, method: %s, status: %d", getRequestID(ctx), r.URL.Path, r.Method, wp.Status())
}

func (a *API) populateConfig(ctx context.Context, w http.ResponseWriter, r *http.Request) context.Context {
	reqID := uuid.NewRandom().String()
	log := a.log.WithFields(logrus.Fields{
		"request_id": reqID,
		"method":     r.Method,
		"path":       r.URL.Path,
	})
	log.Info("Started request")

	ctx = setRequestID(ctx, reqID)
	ctx = setStartTime(ctx, time.Now())
	ctx = setConfig(ctx, a.config)
	ctx = setDB(ctx, a.db)

	ctx = setPayerProxy(ctx, a.payerProxy)

	token, err := extractToken(a.config.JWTSecret, r)
	if err != nil {
		log.WithError(err).Info("Failed to parse token")
		return nil
	}

	if token == nil {
		log.Info("Attempted to make unauthenticated request")
		return nil
	}

	claims := token.Claims.(*JWTClaims)
	adminFlag := false
	for _, g := range claims.Groups {
		if g == a.config.AdminGroupName {
			adminFlag = true
			break
		}
	}
	log = log.WithFields(logrus.Fields{
		"is_admin": adminFlag,
		"user_id":  claims.ID,
	})
	ctx = setAdminFlag(ctx, adminFlag)

	ctx = setToken(ctx, token)
	ctx = setLogger(ctx, log)
	return ctx
}

func extractToken(secret string, r *http.Request) (*jwt.Token, *HTTPError) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, nil
	}

	matches := bearerRegexp.FindStringSubmatch(authHeader)
	if len(matches) != 2 {
		return nil, httpError(http.StatusBadRequest, "Bad authentication header")
	}

	token, err := jwt.ParseWithClaims(matches[1], &JWTClaims{}, func(token *jwt.Token) (interface{}, error) {
		if token.Header["alg"] != jwt.SigningMethodHS256.Name {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, httpError(http.StatusUnauthorized, "Invalid Token")
	}

	claims := token.Claims.(*JWTClaims)
	if claims.StandardClaims.ExpiresAt < time.Now().Unix() {
		return nil, httpError(http.StatusUnauthorized, fmt.Sprintf("Token expired at %v", time.Unix(claims.StandardClaims.ExpiresAt, 0)))
	}
	return token, nil
}

func hello(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	version := getVersion(ctx)
	sendJSON(w, http.StatusOK, map[string]string{
		"version":     version,
		"application": "netlify-subscriptions",
	})
}

func sendJSON(w http.ResponseWriter, status int, obj interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.Encode(obj)
}
