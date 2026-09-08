package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

type closeFunc func()

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("message", strings.ToLower(err.Error())),
	}

	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.String("stack_trace", fmt.Sprintf("%+v", stackErr.StackTrace())))
	}

	customAttrs := linkoerr.Attrs(err)

	if len(customAttrs) > 0 {
		attrs = append(attrs, customAttrs...)
	}

	return attrs
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		multiErr, meOk := errors.AsType[multiError](err)
		if meOk {
			var attrs []slog.Attr

			for i, err := range multiErr.Unwrap() {
				errExtra := errorAttrs(err)
				attrs = append(attrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), errExtra...))
			}

			return slog.GroupAttrs("errors", attrs...)
		}

		errExtra := errorAttrs(err)

		return slog.GroupAttrs("error", errExtra...)

	}
	var sensitiveKeys = []string{"password", "key", "apikey", "secret", "pin", "creditcardno", "user"}

	if slices.Contains(sensitiveKeys, a.Key) {
		a.Value = slog.StringValue("[REDACTED]")
	}
	if a.Value.Kind() == slog.KindString {
		strValue := a.Value.String()
		u, err := url.Parse(strValue)
		if err != nil {
			return a
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				u.User = url.UserPassword(u.User.Username(), "[REDACTED]")
				a.Value = slog.StringValue(u.String())
			}
		}
	}
	return a
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}

	msg := ""
	dangErrCodes := []int{401, 403, 500}

	if slices.Contains(dangErrCodes, status) {
		msg = http.StatusText(status)
	} else {
		msg = err.Error()
	}

	http.Error(w, msg, status)
}

func helperLoggerWith(logger *slog.Logger) *slog.Logger {
	hostname, err := os.Hostname()
	if err != nil {
		log.Println("failed to get hostname: ", err)
		hostname = "unknown"
	}

	env := os.Getenv("ENV")

	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("hostname", hostname),
		slog.String("env", env),
	)

	return logger
}

func initializeLogger() (*slog.Logger, closeFunc, error) {
	isTerm := isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())

	handlers := []slog.Handler{
		tint.NewHandler(os.Stderr, &tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
			NoColor:     !isTerm,
		}),
	}

	cleanup := closeFunc(func() {})

	if logFilePath, ok := os.LookupEnv("LINKO_LOG_FILE"); ok && logFilePath != "" {
		rotator := &lumberjack.Logger{
			Filename:   logFilePath,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}

		handlers = append(handlers, slog.NewJSONHandler(rotator, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		}))

		cleanup = func() {
			if err := rotator.Close(); err != nil {
				log.Printf("error closing log file: %v", err)
			}
		}
	}

	logger := helperLoggerWith(slog.New(slog.NewMultiHandler(handlers...)))
	return logger, cleanup, nil
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {

	tracer_provider, err := initTracing(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize tracer provider: %v\n", err)
		return 1
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := tracer_provider(shutdownCtx); err != nil {
			log.Printf("Error shutting down tracer provider: %v", err)
		}
	}()

	logger, cleanup, err := initializeLogger()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		return 1
	}
	defer cleanup()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Debug(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}
	return 0
}
