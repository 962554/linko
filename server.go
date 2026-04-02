package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"962554/linko/internal/spy"
	"962554/linko/internal/store"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type server struct {
	httpServer *http.Server
	store      store.Store
	logger     *slog.Logger
	cancel     context.CancelFunc
}

func newServer(store store.Store, port int, logger *slog.Logger, cancel context.CancelFunc) *server {
	mux := http.NewServeMux()

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: metricsMiddleware(requestIDHandler()(requestLogger(logger)(mux))),
	}

	s := &server{
		httpServer: srv,
		store:      store,
		logger:     logger,
		cancel:     cancel,
	}

	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)
	mux.Handle("GET /metrics", promhttp.Handler())

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
	port := ln.Addr().(*net.TCPAddr).Port
	s.logger.Debug(fmt.Sprintf("Linko is running on http://localhost:%d\n", port))
	return nil
}

func (s *server) shutdown(ctx context.Context) error {
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

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusInternalServerError {
		err = errors.New(http.StatusText(status))
	}
	http.Error(w, err.Error(), status)
}

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// update r with spy.Reader
			spyReader := &spy.ReadCloser{ReadCloser: r.Body}
			r.Body = spyReader

			// update w with spy.Writer
			spyWriter := &spy.ResponseWriter{ResponseWriter: w}

			r = r.WithContext(context.WithValue(r.Context(), logContextKey, &LogContext{}))

			next.ServeHTTP(spyWriter, r)

			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", redactIP(r.RemoteAddr)),
				slog.Duration("duration", time.Since(start)),
				slog.Int("request_body_bytes", spyReader.BytesRead),
				slog.Int("response_status", spyWriter.StatusCode),
				slog.Int("response_body_bytes", spyWriter.BytesWritten),
				slog.String("request_id", w.Header().Get("X-Request-ID")),
			}

			lc, ok := r.Context().Value(logContextKey).(*LogContext)
			if ok && lc.Username != "" {
				attrs = append(attrs, slog.String("user", lc.Username))
			}

			if ok && lc.Error != nil {
				attrs = append(attrs, slog.Any("error", lc.Error))
			}

			logger.Info("Served request", attrs...)
		})
	}
}

func requestIDHandler() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" {
				requestID = rand.Text()
			}
			w.Header().Set("X-Request-ID", requestID)
			next.ServeHTTP(w, r)
		})
	}
}

func redactIP(hostport string) string {
	fmt.Println("micro:", hostport)
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return host
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}

	ipv4 := ip.To4()
	if ipv4 == nil {
		return host
	}
	return fmt.Sprintf("%d.%d.%d.x", ipv4[0], ipv4[1], ipv4[2])
}
