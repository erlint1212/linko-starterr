package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
		slog.String("message", err.Error()),
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
	return a
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}

	msg := strings.ToLower(err.Error())

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

func initializeLogger(logFile string) (*slog.Logger, error) {
	var (
		handlers []slog.Handler
	)

	replaceAttr := func(groups []string, a slog.Attr) slog.Attr { /* ... */ }

	// First initialize the console logger
	handlers = append(handlers, tint.NewHandler(os.Stderr, &tint.Options{
		ReplaceAttr: replaceAttr,
		NoColor:     !(isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())),
	}))

	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0x666)
		if err != nil {
			return nil, fmt.Errorf("failed to open log file: %w", err)
		}
		bufferedFile := bufio.NewWriter(file)
		handlers = append(handlers, slog.NewJSONHandler(bufferedFile, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
		}))
		defer func() {
			if err := bufferedFile.Flush(); err != nil {
				log.Printf("failed to flush log file: %w", err)
				return
			}
			if err := file.Close(); err != nil {
				log.Printf("failed to close log file: %w", err)
				return
			}
		}
	}

	defer func() error {
		var errs []error
		for _, closer := range closers {
			errs = append(errs, closer())
		}
		return errors.Join(errs...)
	}
	return slog.New(slog.NewMultiHandler(handlers...)), nil
}

func initializeLogger() (*slog.Logger, closeFunc, error) {

	isTerm := isatty.IsTerminal(os.Stderr.Fd()) || isatty.IsCygwinTerminal(os.Stderr.Fd())

	debugHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
		NoColor:     !isTerm,
	})

	logFilePath, exists := os.LookupEnv("LINKO_LOG_FILE")

	if exists {
		multiLoggerFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, func() {}, fmt.Errorf("failed to open log file: %v", err)
		}

		bufferedFile := bufio.NewWriterSize(multiLoggerFile, 8192)

		logger := &lumberjack.Logger{
			Filename:   multiLoggerFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}

		handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
		}))

		cleanup := func() {
			if err := bufferedFile.Flush(); err != nil {
				log.Printf("error flushing buffer to file: %v", err)
			}
			if err := multiLoggerFile.Close(); err != nil {
				log.Printf("error closing log file: %v", err)
			}
		}

		infoHandler := tint.NewHandler(multiLoggerFile, &tint.Options{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
			NoColor:     !isTerm,
		})

		logger := slog.New(slog.NewMultiHandler(
			debugHandler,
			infoHandler,
		))

		logger = helperLoggerWith(logger)

		return logger, cleanup, nil
	}

	logger := slog.New(slog.NewMultiHandler(
		debugHandler,
	))

	logger = helperLoggerWith(logger)

	return logger, func() {}, nil

}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {

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
