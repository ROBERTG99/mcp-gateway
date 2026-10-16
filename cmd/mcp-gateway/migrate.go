package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/ROBERTG99/mcp-gateway/internal/config"
	"github.com/ROBERTG99/mcp-gateway/internal/store"
)

// cmdMigrate applies the embedded goose migrations (default "up") as the schema owner and then
// sets the application role passwords from the environment.
func cmdMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	command := "up"
	if len(args) > 0 {
		command = args[0]
	}
	if err := store.Migrate(ctx, cfg.MigrateDatabaseURL, command, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if command == "up" {
		if err := store.SetRolePasswords(ctx, cfg.MigrateDatabaseURL, map[string]string{
			"mcpgw_app": cfg.AppDBPassword, "mcpgw_readonly": cfg.ReadonlyDBPassword, "mcpgw_upstream": cfg.UpstreamDBPassword,
		}); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}
