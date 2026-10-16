package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
	"github.com/ROBERTG99/mcp-gateway/internal/server"
)

func cmdServe(ctx context.Context, args []string, _, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	upstreams := fs.String("upstreams", envOr("MCPGW_UPSTREAMS_FILE", "config/upstreams.yaml"), "upstreams YAML file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	raw, err := os.ReadFile(*upstreams)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read upstreams: %v\n", err)
		return 1
	}
	var file struct {
		Upstreams []catalog.UpstreamConfig `yaml:"upstreams"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		_, _ = fmt.Fprintf(stderr, "parse upstreams: %v\n", err)
		return 1
	}
	cfg := server.Config{
		MCPAddr: ":8210", AdminAddr: ":8200", Upstreams: file.Upstreams,
		CatalogRefresh: 30 * time.Second, ShutdownTimeout: 10 * time.Second,
	}
	if err := server.Run(ctx, cfg, logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
