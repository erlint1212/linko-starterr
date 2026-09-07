package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"

	"net/http/pprof"
	_ "net/http/pprof"

	"boot.dev/linko/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// httpRequestsTotal counts requests by method, path and status.
var httpRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	},
	[]string{"method", "path", "status"},
)

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(rec, r)

		path := r.URL.Path
		method := r.Method
		status := strconv.Itoa(rec.status)

		httpRequestsTotal.
			WithLabelValues(method, path, status).
			Inc()
	})
}

func newServer(store store.Store, port int, cancel context.CancelFunc, logger *slog.Logger) *server {
	mux := http.NewServeMux()

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: requestLogger(logger)(requestID(metricsMiddleware(mux))),
	}

	s := &server{
		httpServer: srv,
		store:      store,
		cancel:     cancel,
		logger:     logger,
	}

	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.Handle("GET /debug/pprof/", s.authMiddleware(http.HandlerFunc(pprof.Index)))
	mux.Handle("GET /debug/pprof/profile", s.authMiddleware(http.HandlerFunc(pprof.Profile)))

	return s
}

func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("network port not recognized")
	}
	s.logger.Info(fmt.Sprintf("Linko is running on http://localhost:%d", tcpAddr.Port))
	return nil
}

func (s *server) shutdown(ctx context.Context) error {
	s.logger.Debug("Linko is shutting down")
	return s.httpServer.Shutdown(ctx)
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
}
