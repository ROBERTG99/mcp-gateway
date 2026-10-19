// Package version reports the build version of the mcp-gateway binaries.
package version

import "runtime/debug"

// These values are set at link time with -ldflags "-X ...". They are the only
// package-level variables in the module and are never written at runtime.
var (
	version = "dev"
	commit  = ""
)

// Version returns the semantic version of the build ("dev" for local builds).
func Version() string { return version }

// Commit returns the VCS revision the binary was built from, if known.
func Commit() string {
	if commit != "" {
		return commit
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}

// String renders "mcp-gateway <version> (<commit>)".
func String() string { return "mcp-gateway " + Version() + " (" + Commit() + ")" }
