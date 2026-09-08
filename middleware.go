package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

const logContextKey contextKey = "log_context"
const requestIDKey contextKey = "request_id"

type LogContext struct {
	Username string
	Error    error
}

func initTracing(ctx context.Context) (func(context.Context) error, error) {
	exp, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp,
			sdktrace.WithBatchTimeout(2*time.Second),
		),
		sdktrace.WithResource(resource.Default()),
	)

	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func (r *spyReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.bytesRead += n
	return n, err
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n
	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			start := time.Now()

			spyWriter := &spyResponseWriter{ResponseWriter: w}

			spyReader := &spyReadCloser{ReadCloser: r.Body}
			r.Body = spyReader

			logCtx := &LogContext{}

			ctx := context.WithValue(r.Context(), logContextKey, logCtx)
			r = r.WithContext(ctx)

			next.ServeHTTP(spyWriter, r)

			reqID, ok := r.Context().Value(requestIDKey).(string)
			if !ok {
				reqID = "unknown"
			}

			logArgs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", redactIP(r.RemoteAddr)),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", reqID),
			}

			if logCtx.Username != "" {
				logArgs = append(logArgs, slog.String("user", logCtx.Username))
			}

			if logCtx.Error != nil {
				logArgs = append(logArgs, slog.Any("error", logCtx.Error))
			}

			logger.Info("Served request", logArgs...)
		})
	}
}

func redactIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return addr
	}

	ip4 := ip.To4()
	if ip4 == nil {
		return ip.String()
	}

	return fmt.Sprintf("%d.%d.%d.x", ip4[0], ip4[1], ip4[2])
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		requestID := r.Header.Get("X-Request-ID")

		if requestID == "" {
			requestID = rand.Text()
		}

		w.Header().Set("X-Request-ID", requestID)

		ctx := context.WithValue(r.Context(), requestIDKey, requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
