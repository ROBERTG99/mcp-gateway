package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/format"

	"github.com/ROBERTG99/mcp-gateway/internal/policy"
)

// cmdPolicy implements `policy test <dir>` and `policy fmt [--check] <dir|file>...`.
func cmdPolicy(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: mcp-gateway policy test <dir> | policy fmt [--check] <path>...")
		return 2
	}
	switch args[0] {
	case "test":
		return policyTest(ctx, args[1:], stdout, stderr)
	case "fmt":
		return policyFmt(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "unknown policy command %q\n", args[0])
		return 2
	}
}

func policyTest(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	dir := "policies"
	if len(args) > 0 {
		dir = args[0]
	}
	results, err := policy.RunTests(ctx, dir)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	failed := 0
	for _, r := range results {
		status := "PASS"
		if !r.Pass {
			status, failed = "FAIL", failed+1
		}
		_, _ = fmt.Fprintf(stdout, "%s %s", status, r.Name)
		if r.Error != "" {
			_, _ = fmt.Fprintf(stdout, ": %s", r.Error)
		}
		_, _ = fmt.Fprintln(stdout)
	}
	_, _ = fmt.Fprintf(stdout, "%d tests, %d passed, %d failed\n", len(results), len(results)-failed, failed)
	if failed > 0 || len(results) == 0 {
		return 1
	}
	return 0
}

func policyFmt(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("policy fmt", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "report unformatted files instead of rewriting them")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	paths := fs.Args()
	if len(paths) == 0 {
		paths = []string{"policies"}
	}
	unformatted := 0
	for _, path := range paths {
		n, err := formatTree(path, *check, stdout)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		unformatted += n
	}
	if unformatted > 0 {
		_, _ = fmt.Fprintf(stderr, "%d Rego file(s) need formatting (run make fmt)\n", unformatted)
		return 1
	}
	return 0
}

// formatTree formats one .rego file or every .rego file under a directory. Directory traversal
// goes through os.Root so symlinks cannot escape the tree. It returns the number of files that
// need formatting (check mode) or were rewritten.
func formatTree(path string, check bool, stdout io.Writer) (int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	dir, files := path, []string{"."}
	if !info.IsDir() {
		dir, files = filepath.Dir(path), []string{filepath.Base(path)}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = root.Close() }()
	if info.IsDir() {
		files = nil
		err := fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".rego") {
				files = append(files, p)
			}
			return err
		})
		if err != nil {
			return 0, fmt.Errorf("walk %s: %w", dir, err)
		}
	}
	changed := 0
	for _, f := range files {
		src, err := root.ReadFile(f)
		if err != nil {
			return changed, fmt.Errorf("read %s: %w", f, err)
		}
		out, err := format.SourceWithOpts(f, src, format.Opts{RegoVersion: ast.RegoV1})
		if err != nil {
			return changed, fmt.Errorf("format %s: %w", filepath.Join(dir, f), err)
		}
		if bytes.Equal(src, out) {
			continue
		}
		changed++
		if check {
			_, _ = fmt.Fprintln(stdout, filepath.Join(dir, f))
			continue
		}
		if err := root.WriteFile(f, out, 0o600); err != nil {
			return changed, fmt.Errorf("write %s: %w", f, err)
		}
	}
	if !check {
		return 0, nil
	}
	return changed, nil
}
