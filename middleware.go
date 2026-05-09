package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"
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

type LogContext struct {
	Username string
	Error    error
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

			logArgs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", r.RemoteAddr),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				slog.Duration("duration", time.Since(start)),
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
