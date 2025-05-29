// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/profiler"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"go.elastic.co/apm"
	"go.elastic.co/apm/module/apmhttp"
	"google.golang.org/grpc"
)

const (
	port            = "8080"
	defaultCurrency = "USD"
	cookieMaxAge    = 60 * 60 * 48

	cookiePrefix    = "shop_"
	cookieSessionID = cookiePrefix + "session-id"
	cookieCurrency  = cookiePrefix + "currency"
)

var (
	whitelistedCurrencies = map[string]bool{
		"USD": true,
		"EUR": true,
		"CAD": true,
		"JPY": true,
		"GBP": true,
		"TRY": true,
	}

	baseUrl = ""
)

type ctxKeySessionID struct{}

type frontendServer struct {
	productCatalogSvcAddr string
	productCatalogSvcConn *grpc.ClientConn

	currencySvcAddr string
	currencySvcConn *grpc.ClientConn

	cartSvcAddr string
	cartSvcConn *grpc.ClientConn

	recommendationSvcAddr string
	recommendationSvcConn *grpc.ClientConn

	checkoutSvcAddr string
	checkoutSvcConn *grpc.ClientConn

	shippingSvcAddr string
	shippingSvcConn *grpc.ClientConn

	adSvcAddr string
	adSvcConn *grpc.ClientConn

	collectorAddr string
	collectorConn *grpc.ClientConn

	shoppingAssistantSvcAddr string
}

func main() {
	ctx := context.Background()

	// Setup structured logging
	log := logrus.New()
	log.Level = logrus.DebugLevel
	log.Formatter = &logrus.JSONFormatter{
		FieldMap: logrus.FieldMap{
			logrus.FieldKeyTime:  "timestamp",
			logrus.FieldKeyLevel: "severity",
			logrus.FieldKeyMsg:   "message",
		},
		TimestampFormat: time.RFC3339Nano,
	}
	log.Out = os.Stdout

	// Initialize Elastic APM agent BEFORE you create HTTP server
	// Elastic APM config is read from environment variables like:
	// - ELASTIC_APM_SERVICE_NAME (e.g., "frontend")
	// - ELASTIC_APM_SERVER_URL (e.g., "http://apm-server:8200")
	// - ELASTIC_APM_SECRET_TOKEN (if your server requires it)
	// - ELASTIC_APM_ENVIRONMENT (optional)
	// - ELASTIC_APM_METRICS_INTERVAL (e.g., "10s")
	//
	// You can also set ELASTIC_APM_LOG_LEVEL=debug to troubleshoot
	if apm.DefaultTracer == nil {
		panic("elastic apm tracer not initialized")
	}
	// The DefaultTracer is automatically initialized when imported.
	// Just ensure your env vars are set before running.

	svc := new(frontendServer)

	baseUrl = os.Getenv("BASE_URL")

	srvPort := port
	if os.Getenv("PORT") != "" {
		srvPort = os.Getenv("PORT")
	}

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = "0.0.0.0" // default listen address if not set
	}

	// Read gRPC service addresses from environment variables
	mustMapEnv(&svc.productCatalogSvcAddr, "PRODUCT_CATALOG_SERVICE_ADDR")
	mustMapEnv(&svc.currencySvcAddr, "CURRENCY_SERVICE_ADDR")
	mustMapEnv(&svc.cartSvcAddr, "CART_SERVICE_ADDR")
	mustMapEnv(&svc.recommendationSvcAddr, "RECOMMENDATION_SERVICE_ADDR")
	mustMapEnv(&svc.checkoutSvcAddr, "CHECKOUT_SERVICE_ADDR")
	mustMapEnv(&svc.shippingSvcAddr, "SHIPPING_SERVICE_ADDR")
	mustMapEnv(&svc.adSvcAddr, "AD_SERVICE_ADDR")
	mustMapEnv(&svc.shoppingAssistantSvcAddr, "SHOPPING_ASSISTANT_SERVICE_ADDR")

	// Connect gRPC clients
	mustConnGRPC(ctx, &svc.currencySvcConn, svc.currencySvcAddr)
	mustConnGRPC(ctx, &svc.productCatalogSvcConn, svc.productCatalogSvcAddr)
	mustConnGRPC(ctx, &svc.cartSvcConn, svc.cartSvcAddr)
	mustConnGRPC(ctx, &svc.recommendationSvcConn, svc.recommendationSvcAddr)
	mustConnGRPC(ctx, &svc.shippingSvcConn, svc.shippingSvcAddr)
	mustConnGRPC(ctx, &svc.checkoutSvcConn, svc.checkoutSvcAddr)
	mustConnGRPC(ctx, &svc.adSvcConn, svc.adSvcAddr)

	// Setup router with your handlers
	r := mux.NewRouter()
	r.HandleFunc(baseUrl+"/", svc.homeHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/product/{id}", svc.productHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/cart", svc.viewCartHandler).Methods(http.MethodGet, http.MethodHead)
	r.HandleFunc(baseUrl+"/cart", svc.addToCartHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/cart/empty", svc.emptyCartHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/setCurrency", svc.setCurrencyHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/logout", svc.logoutHandler).Methods(http.MethodGet)
	r.HandleFunc(baseUrl+"/cart/checkout", svc.placeOrderHandler).Methods(http.MethodPost)
	r.HandleFunc(baseUrl+"/assistant", svc.assistantHandler).Methods(http.MethodGet)
	r.PathPrefix(baseUrl + "/static/").Handler(http.StripPrefix(baseUrl+"/static/", http.FileServer(http.Dir("./static/"))))
	r.HandleFunc(baseUrl+"/robots.txt", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "User-agent: *\nDisallow: /") })
	r.HandleFunc(baseUrl+"/_healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	r.HandleFunc(baseUrl+"/product-meta/{ids}", svc.getProductByID).Methods(http.MethodGet)
	r.HandleFunc(baseUrl+"/bot", svc.chatBotHandler).Methods(http.MethodPost)

	// Wrap router with Elastic APM middleware to instrument HTTP requests
	var handler http.Handler = apmhttp.Wrap(r)

	// Add logging middleware and session ID middleware as before
	handler = &logHandler{log: log, next: handler}
	handler = ensureSessionID(handler)

	log.Infof("starting server on %s:%s", addr, srvPort)
	log.Fatal(http.ListenAndServe(addr+":"+srvPort, handler))
}

func mustMapEnv(target *string, envKey string) {
	v := os.Getenv(envKey)
	if v == "" {
		panic(fmt.Sprintf("environment variable %q not set", envKey))
	}
	*target = v
}

func mustConnGRPC(ctx context.Context, conn **grpc.ClientConn, addr string) {
	var err error
	ctx, cancel := context.WithTimeout(ctx, time.Second*3)
	defer cancel()
	*conn, err = grpc.DialContext(ctx, addr,
		grpc.WithInsecure(),
		grpc.WithUnaryInterceptor(), // No OTel here, just pure gRPC
		grpc.WithStreamInterceptor())
	if err != nil {
		panic(errors.Wrapf(err, "grpc: failed to connect %s", addr))
	}
}

// Below are your handler method stubs.
// Implement these handlers in your codebase.

func (s *frontendServer) homeHandler(w http.ResponseWriter, r *http.Request)          {}
func (s *frontendServer) productHandler(w http.ResponseWriter, r *http.Request)       {}
func (s *frontendServer) viewCartHandler(w http.ResponseWriter, r *http.Request)      {}
func (s *frontendServer) addToCartHandler(w http.ResponseWriter, r *http.Request)     {}
func (s *frontendServer) emptyCartHandler(w http.ResponseWriter, r *http.Request)     {}
func (s *frontendServer) setCurrencyHandler(w http.ResponseWriter, r *http.Request)   {}
func (s *frontendServer) logoutHandler(w http.ResponseWriter, r *http.Request)        {}
func (s *frontendServer) placeOrderHandler(w http.ResponseWriter, r *http.Request)    {}
func (s *frontendServer) assistantHandler(w http.ResponseWriter, r *http.Request)     {}
func (s *frontendServer) getProductByID(w http.ResponseWriter, r *http.Request)       {}
func (s *frontendServer) chatBotHandler(w http.ResponseWriter, r *http.Request)       {}

// Middleware types for logging and session - implement as you had previously

type logHandler struct {
	log  *logrus.Logger
	next http.Handler
}

func (h *logHandler) ServeHTTP
