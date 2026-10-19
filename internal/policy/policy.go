// Package policy embeds OPA: it compiles the Rego bundle in policies/ into one prepared query,
// data.mcpgw.authz.decision, evaluates it per call, and hot-reloads atomically. Without a
// successfully compiled bundle every evaluation denies (fail closed) and Ready reports false.
package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/storage"
	"github.com/open-policy-agent/opa/v1/storage/inmem"
	"github.com/open-policy-agent/opa/v1/tester"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// Query is the single entry point of the bundle.
const Query = "data.mcpgw.authz.decision"

// ErrNotLoaded is returned when no bundle has compiled yet.
var ErrNotLoaded = errors.New("policy bundle not loaded")

// Approval is the approval state passed to the policy (only after binding checks).
type Approval struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// Input is the policy input contract (spec §4.4). Args is nil for tools/list.
type Input struct {
	Tenant      string         `json:"tenant"`
	Agent       string         `json:"agent"`
	Scopes      []string       `json:"scopes"`
	Server      string         `json:"server"`
	Tool        string         `json:"tool"`
	Args        any            `json:"args"`
	Annotations map[string]any `json:"annotations"`
	Approval    *Approval      `json:"approval"`
	Now         time.Time      `json:"now"`
}

// Decision is the policy output contract.
type Decision struct {
	Allow           bool     `json:"allow"`
	RequireApproval bool     `json:"require_approval"`
	Reasons         []string `json:"reasons"`
}

// Denied is the fail-closed decision used when no bundle is loaded or evaluation fails.
func Denied(reason string) Decision { return Decision{Reasons: []string{reason}} }

// Options configure an Engine.
type Options struct {
	Dir   string
	Clock clock.Clock // fills Input.Now when unset (default: wall clock)
	// OnFailure is called for every failed load or reload, the initial one included (metrics).
	OnFailure func(error)
}

type compiled struct {
	query rego.PreparedEvalQuery
	hash  string
}

// Engine evaluates the policy bundle. Safe for concurrent use.
type Engine struct {
	opts    Options
	current atomic.Pointer[compiled]

	mu        sync.Mutex
	lastErr   string
	listeners []func()
	reloadMu  sync.Mutex
}

// New compiles the bundle in opts.Dir. On failure it returns the (not ready, deny-all) engine
// together with the error, so the gateway can start, report not-ready and keep retrying.
func New(ctx context.Context, opts Options) (*Engine, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	e := &Engine{opts: opts}
	return e, e.Reload(ctx)
}

// OnReload registers fn, called after every successful reload.
func (e *Engine) OnReload(fn func()) {
	e.mu.Lock()
	e.listeners = append(e.listeners, fn)
	e.mu.Unlock()
}

// Ready reports whether a bundle is loaded.
func (e *Engine) Ready() bool { return e.current.Load() != nil }

// BundleHash is the SHA-256 of the loaded bundle's files ("" when none).
func (e *Engine) BundleHash() string {
	if c := e.current.Load(); c != nil {
		return c.hash
	}
	return ""
}

// LastError is the error of the most recent failed load ("" after a success).
func (e *Engine) LastError() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastErr
}

// Reload recompiles the bundle and swaps it in atomically. A failed reload keeps the previous
// bundle (if any) and records the error.
func (e *Engine) Reload(ctx context.Context) error {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	b, err := LoadBundle(e.opts.Dir, false)
	if err == nil {
		var q rego.PreparedEvalQuery
		q, err = b.prepare(ctx)
		if err == nil {
			e.current.Store(&compiled{query: q, hash: b.Hash})
			e.mu.Lock()
			e.lastErr = ""
			listeners := slices.Clone(e.listeners)
			e.mu.Unlock()
			for _, fn := range listeners {
				fn()
			}
			return nil
		}
	}
	e.mu.Lock()
	e.lastErr = err.Error()
	e.mu.Unlock()
	if e.opts.OnFailure != nil {
		e.opts.OnFailure(err)
	}
	return fmt.Errorf("policy reload: %w", err)
}

// Watch reloads when the bundle's content hash changes, polling every interval.
func (e *Engine) Watch(ctx context.Context, interval time.Duration, onError func(error)) error {
	if interval <= 0 {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			b, err := LoadBundle(e.opts.Dir, false)
			if err == nil && b.Hash == e.BundleHash() {
				continue
			}
			if err := e.Reload(ctx); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}

// Evaluate returns the decision for in. Without a bundle, or on evaluation errors, it returns a
// deny decision together with the error (fail closed).
func (e *Engine) Evaluate(ctx context.Context, in Input) (Decision, error) {
	c := e.current.Load()
	if c == nil {
		return Denied("policy_unavailable"), ErrNotLoaded
	}
	if in.Now.IsZero() {
		in.Now = e.opts.Clock.Now().UTC()
	}
	value, err := inputValue(in)
	if err != nil {
		return Denied("policy_error"), err
	}
	rs, err := c.query.Eval(ctx, rego.EvalParsedInput(value))
	if err != nil {
		return Denied("policy_error"), fmt.Errorf("evaluate policy: %w", err)
	}
	if len(rs) != 1 || len(rs[0].Expressions) != 1 {
		return Denied("policy_error"), errors.New("policy returned no decision")
	}
	return decode(rs[0].Expressions[0].Value)
}

func inputValue(in Input) (ast.Value, error) {
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("encode policy input: %w", err)
	}
	v, err := ast.ValueFromReader(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("parse policy input: %w", err)
	}
	return v, nil
}

func decode(v any) (Decision, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return Denied("policy_error"), fmt.Errorf("decision is %T, want object", v)
	}
	var d Decision
	allow, ok1 := m["allow"].(bool)
	hold, ok2 := m["require_approval"].(bool)
	reasons, ok3 := m["reasons"].([]any)
	if !ok1 || !ok2 || !ok3 || len(m) != 3 {
		return Denied("policy_error"), fmt.Errorf("decision %v violates {allow, require_approval, reasons}", m)
	}
	d.Allow, d.RequireApproval = allow, hold
	d.Reasons = make([]string, 0, len(reasons))
	for _, r := range reasons {
		s, ok := r.(string)
		if !ok {
			return Denied("policy_error"), fmt.Errorf("reason %v is not a string", r)
		}
		d.Reasons = append(d.Reasons, s)
	}
	if d.Allow && d.RequireApproval {
		return Denied("policy_error"), errors.New("decision both allows and requires approval")
	}
	return d, nil
}

// Bundle is the parsed content of a policy directory.
type Bundle struct {
	Modules map[string]string // path -> Rego source
	Data    map[string]any    // data.mcpgw.<file basename>
	Hash    string
}

// LoadBundle reads *.rego (test files only when withTests) and data/*.json from dir. Hidden
// directories are skipped: a Kubernetes ConfigMap volume keeps its real files in a hidden
// timestamped directory and exposes them through top-level symlinks, which are read instead.
func LoadBundle(dir string, withTests bool) (*Bundle, error) {
	b := &Bundle{Modules: map[string]string{}, Data: map[string]any{}}
	h := sha256.New()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && (strings.HasSuffix(path, ".rego") || strings.HasSuffix(path, ".json")) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read policy dir: %w", err)
	}
	slices.Sort(files)
	for _, path := range files {
		isTest := strings.HasSuffix(path, "_test.rego")
		if isTest && !withTests {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: files under the configured policy dir
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		rel, _ := filepath.Rel(dir, path)
		if !isTest {
			_, _ = h.Write([]byte(filepath.ToSlash(rel) + "\x00"))
			_, _ = h.Write(src)
		}
		if strings.HasSuffix(path, ".rego") {
			b.Modules[filepath.ToSlash(rel)] = string(src)
			continue
		}
		var v any
		if err := json.Unmarshal(src, &v); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		b.Data[strings.TrimSuffix(filepath.Base(path), ".json")] = v
	}
	if len(b.Modules) == 0 {
		return nil, fmt.Errorf("no .rego files in %s", dir)
	}
	b.Hash = hex.EncodeToString(h.Sum(nil))
	return b, nil
}

func (b *Bundle) store() storage.Store {
	return inmem.NewFromObject(map[string]any{"mcpgw": b.Data})
}

// capabilities are OPA's builtins minus those OPA marks non-deterministic (http.send,
// net.lookup_ip_addr, time.now_ns, rand.intn, uuid.rfc4122, io.jwt.decode_verify, opa.runtime and
// others) and the debugging output builtins: decisions must depend on the input and the bundle only
// (.claude/rules/rego.md), so a bundle that uses them fails to compile. The gateway and `policy test`
// both compile with them.
func capabilities() *ast.Capabilities {
	c := ast.CapabilitiesForThisVersion()
	c.Builtins = slices.DeleteFunc(slices.Clone(c.Builtins), func(b *ast.Builtin) bool {
		return b.Nondeterministic || b.Name == "print" || b.Name == "internal.print" || b.Name == "trace"
	})
	return c
}

func (b *Bundle) prepare(ctx context.Context) (rego.PreparedEvalQuery, error) {
	opts := []func(*rego.Rego){rego.Query(Query), rego.Store(b.store()), rego.StrictBuiltinErrors(true), rego.Capabilities(capabilities())}
	for name, src := range b.Modules {
		opts = append(opts, rego.Module(name, src))
	}
	q, err := rego.New(opts...).PrepareForEval(ctx)
	if err != nil {
		return rego.PreparedEvalQuery{}, fmt.Errorf("compile policy: %w", err)
	}
	return q, nil
}

// TestResult is the outcome of one Rego unit test.
type TestResult struct {
	Name  string
	Pass  bool
	Error string
}

// RunTests runs every test_* rule in the bundle's *_test.rego files.
func RunTests(ctx context.Context, dir string) ([]TestResult, error) {
	b, err := LoadBundle(dir, true)
	if err != nil {
		return nil, err
	}
	modules := map[string]*ast.Module{}
	for name, src := range b.Modules {
		m, err := ast.ParseModuleWithOpts(name, src, ast.ParserOptions{RegoVersion: ast.RegoV1})
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		modules[name] = m
	}
	store := b.store()
	txn, err := store.NewTransaction(ctx)
	if err != nil {
		return nil, fmt.Errorf("transaction: %w", err)
	}
	defer store.Abort(ctx, txn)
	compiler := ast.NewCompiler().WithCapabilities(capabilities())
	ch, err := tester.NewRunner().SetCompiler(compiler).SetStore(store).SetModules(modules).RunTests(ctx, txn)
	if err != nil {
		return nil, fmt.Errorf("run rego tests: %w", err)
	}
	var out []TestResult
	for r := range ch {
		tr := TestResult{Name: r.Package + "." + r.Name, Pass: !r.Fail && r.Error == nil}
		if r.Error != nil {
			tr.Error = r.Error.Error()
		} else if r.Fail {
			tr.Error = "assertion failed"
		}
		out = append(out, tr)
	}
	slices.SortFunc(out, func(a, b TestResult) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}
