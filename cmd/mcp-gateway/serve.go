package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/server"
)

func cmdServe(ctx context.Context, _ []string, _, stderr io.Writer) int {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	logger := newLogger(stderr, cfg.LogLevel)
	if err := server.Run(ctx, cfg, logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		return 1
	}
	return 0
}

func newLogger(w io.Writer, level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: l}))
}
