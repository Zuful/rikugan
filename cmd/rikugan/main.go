// Command rikugan starts the Rikugan MCP server over STDIO transport.
// "rikugan serve" starts the proactive mode's daemon instead, and
// "rikugan doctor" checks its configuration end to end
// ("rikugan doctor --send-test" also sends a test message on every
// channel and pings the heartbeat).
//
// Logs are written as structured JSON to stderr (the STDIO MCP transport
// uses stdin/stdout for the JSON-RPC protocol). Under serve, stdout
// carries one JSON line per verdict, for log pipelines.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/Zuful/rikugan/internal/app"
	"github.com/Zuful/rikugan/pkg/logging"
)

// version is set at build-time via -ldflags.
var version = "0.1.0"

func main() {
	logger := logging.New("RIKUGAN_LOG_LEVEL")
	slog.SetDefault(logger)

	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		os.Exit(doctor(logger))
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(logger); err != nil {
			logger.Error("rikugan serve exited with error", slog.String("error", err.Error()))
			os.Exit(1)
		}
		return
	}
	if err := run(logger); err != nil {
		logger.Error("server exited with error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// run wires the cancellable context, asks the app package to build the
// MCP server with every available provider, and serves it over STDIO
// until the listener exits or a signal fires.
func run(logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer cancel()

	mcpSrv, err := app.Setup(logger, version)
	if err != nil {
		return err
	}

	stdio := server.NewStdioServer(mcpSrv)
	logger.Info("rikugan starting",
		slog.String("transport", "stdio"),
		slog.String("version", version),
	)

	errCh := make(chan error, 1)
	go func() {
		errCh <- stdio.Listen(ctx, os.Stdin, os.Stdout)
	}()

	select {
	case <-ctx.Done():
		logger.Info("received shutdown signal, exiting")
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("stdio listener: %w", err)
		}
		return nil
	}
}

// serve runs the daemon until SIGINT or SIGTERM.
func serve(logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	verdicts := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("rikugan serve", slog.String("version", version))
	return app.Serve(ctx, logger, verdicts)
}

// doctor checks rikugan serve's configuration and returns the exit
// code: 1 when a check failed, 2 on a bad argument.
func doctor(logger *slog.Logger) int {
	opts := app.DoctorOptions{Logger: logger}
	for _, arg := range os.Args[2:] {
		switch arg {
		case "--send-test":
			opts.SendTest = true
		default:
			fmt.Fprintln(os.Stderr, "usage: rikugan doctor [--send-test]")
			return 2
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !app.Doctor(ctx, os.Stdout, opts) {
		return 1
	}
	return 0
}
