// Package migrations embeds the goose SQL migrations for the gateway and example upstreams.
package migrations

import "embed"

// FS holds every *.sql migration. Package-level by necessity: go:embed only targets variables.
//
//go:embed *.sql
var FS embed.FS
