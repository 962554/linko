package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"962554/linko/internal/build"
	"962554/linko/internal/linkoerr"
	"962554/linko/internal/store"

	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type closeFunc func() error

var tracer trace.Tracer

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

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	bufSize := 8192
	logFile := "LINKO_LOG_FILE"

	shutdownTracing, err := initTracing(ctx)
	if err != nil {
		return 1
	}
	defer func() {
		err := shutdownTracing(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to shutdown tracin: %v", err)
		}
	}()

	logger, closer, err := initializeLogger(os.Getenv(logFile), bufSize)
	if err != nil {
		return 1
	}
	defer func() {
		err := closer()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to close logger: %v", err)
		}
	}()

	env := os.Getenv("ENV")
	hostname, err := os.Hostname()
	if err != nil {
		logger.Debug("error getting hostname")
	}

	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error("failed to create store", "error", err)
		return 1
	}
	s := newServer(*st, httpPort, logger, cancel)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	logger.Debug("Linko is shutting down")
	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown server", "error", err)
		return 1
	}
	if serverErr != nil {
		logger.Error("server error", "error", serverErr)
		return 1
	}
	return 0
}

func initializeLogger(logFile string, bufSize int) (*slog.Logger, closeFunc, error) {
	debugHandler := tint.NewHandler(os.Stderr, &tint.Options{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
		NoColor:     !(isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())),
	})

	handlers := []slog.Handler{debugHandler}

	if logFile != "" {
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
		}))

		close := func() error {
			logger.Close()
			return nil
		}
		return slog.New(slog.NewMultiHandler(handlers...)), close, nil
	}
	return slog.New(debugHandler), func() error { return nil }, nil
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	sensitiveKeys := []string{"user", "password", "key", "apikey", "secret", "pin", "creditcardno"}
	// Replace any potentially sensitive values with the string [REDACTED]
	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Key == "long_url" {
		long_url := a.Value.String()
		url, err := url.Parse(long_url)
		if err != nil {
			return a
		}
		if password, ok := url.User.Password(); ok {
			fmt.Println("micro:", a.Value.String(), password, url.User.String())
			a.Value = slog.StringValue(strings.Replace(long_url, password, "[REDACTED]", 1))
		}

	}

	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		if me, ok := a.Value.Any().(multiError); ok {
			var errAttrs []slog.Attr
			for i, err := range me.Unwrap() {
				group := slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), linkoerr.Attrs(err)...)
				errAttrs = append(errAttrs, group)
			}
			return slog.GroupAttrs("errors", errAttrs...)
		}
		attrs := []slog.Attr{
			{
				Key:   "message",
				Value: slog.StringValue(err.Error()),
			},
		}

		if stackErr, ok := errors.AsType[stackTracer](err); ok {
			attrs = append(attrs, linkoerr.Attrs(err)...)
			attrs = append(attrs, slog.Attr{
				Key:   "stack_trace",
				Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
			})
		}
		return slog.GroupAttrs("error", attrs...)
	}
	return a
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
	tracer = tp.Tracer("boot.dev/linko")
	return tp.Shutdown, nil
}
