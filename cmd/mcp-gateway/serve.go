package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/redact"
	"github.com/ROBERTG99/mcp-gateway/internal/server"
	"github.com/ROBERTG99/mcp-gateway/internal/telemetry"
	"github.com/ROBERTG99/mcp-gateway/internal/version"
)

func cmdServe(ctx context.Context, _ []string, _, stderr io.Writer) int {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	logger := newLogger(stderr, cfg.LogLevel)
	shutdown, err := telemetry.Setup(ctx, telemetry.Options{Endpoint: cfg.OTelEndpoint, Node: cfg.Node, Version: version.Version()})
	if err != nil {
		logger.Error("telemetry", "error", err)
		return 1
	}
	defer func() { _ = shutdown(context.WithoutCancel(ctx)) }()
	if err := server.Run(ctx, cfg, logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		return 1
	}
	return 0
}

func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	// Every log line passes through the redactor (NFR-4: no secrets, tokens or raw PII in logs).
	// trace_id and span_id are added from the context of each record.
	return slog.New(redact.NewHandler(telemetry.TraceHandler{Handler: slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l})}, redact.New()))
}
