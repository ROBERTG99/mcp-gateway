// Package loadgen drives the gateway over MCP for the chaos eval (spec §11, chaos.v1) and the
// load test. The chaos run sends refunds with idempotency keys while the orders upstream stalls a
// fraction of them past the gateway timeout; the client retries like a real agent would, and a
// share of keys is deliberately sent twice. Ground truth comes from the upstream itself
// (orders.get_order refunded_cents before and after) and from the audit log (admin API).
package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/demo"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
	"github.com/ROBERTG99/mcp-gateway/internal/mcperr"
)

// ChaosConfig configures a chaos run.
type ChaosConfig struct {
	GatewayURL, IssuerURL, AdminURL string
	Calls                           int           // logical refund calls (distinct idempotency keys)
	Concurrency                     int           // parallel workers (default 64)
	Sessions                        int           // MCP sessions shared by the workers (default 8)
	RetryShare                      float64       // share of keys sent a second time on purpose (default 0.2)
	KeyBudget                       time.Duration // how long a key is retried on timeouts, in-progress and an open breaker (default 2 min)
	SettleWait                      time.Duration // wait after the last call before reading the orders (>= the upstream stall; default 15 s)
	Orders                          int           // t_alpha orders the refunds are spread over (default: all eligible)
	RunID                           string
	HTTP                            *http.Client
	Out                             io.Writer
	Seed                            uint64 // datagen seed of the seeded stack
}

// ChaosResult is the outcome of a run.
type ChaosResult struct {
	RunID           string        `json:"run_id"`
	Agent           string        `json:"agent"`
	Calls           int           `json:"calls"`
	Attempts        int64         `json:"attempts"`
	Retries         int64         `json:"retries"`
	DeliberateDups  int64         `json:"deliberate_duplicates"`
	Timeouts        int64         `json:"timeouts"`        // -32046 after waiting (the upstream call timed out)
	FastFails       int64         `json:"breaker_rejects"` // -32046 within 250 ms (breaker open)
	InProgress      int64         `json:"in_progress"`
	OtherErrors     int64         `json:"other_errors"`
	Succeeded       int64         `json:"succeeded"`
	Replayed        int64         `json:"replayed"`
	Duplicates      int64         `json:"duplicates"`
	Lost            int64         `json:"lost"`
	Unanswered      int64         `json:"unanswered"` // keys (and re-sends) that never got a success within the key budget
	AuditEvents     int64         `json:"audit_events"`
	AuditedRatio    float64       `json:"audited_ratio"`
	ChainOK         bool          `json:"chain_ok"`
	Duration        time.Duration `json:"duration_ns"`
	LatencyP50MS    float64       `json:"latency_p50_ms"`
	LatencyP95MS    float64       `json:"latency_p95_ms"`
	PerOrder        []OrderResult `json:"per_order"`
	TimeoutShare    float64       `json:"timeout_share"`
	FirstOtherError string        `json:"first_other_error,omitempty"`
}

// OrderResult compares one order's refunds with the keys sent to it.
type OrderResult struct {
	OrderID    string `json:"order_id"`
	Keys       int64  `json:"keys"`
	Increase   int64  `json:"increase_cents"`
	Duplicates int64  `json:"duplicates"`
}

func (c *ChaosConfig) defaults() {
	if c.Concurrency <= 0 {
		c.Concurrency = 64
	}
	if c.Sessions <= 0 {
		c.Sessions = 8
	}
	if c.RetryShare == 0 {
		c.RetryShare = 0.2
	}
	if c.KeyBudget <= 0 {
		c.KeyBudget = 2 * time.Minute
	}
	if c.SettleWait <= 0 {
		c.SettleWait = 15 * time.Second
	}
	if c.Orders <= 0 {
		c.Orders = 1 << 30
	}
	if c.RunID == "" {
		c.RunID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if c.Out == nil {
		c.Out = io.Discard
	}
	if c.Seed == 0 {
		c.Seed = datagen.DefaultSeed
	}
}

// chaosOrders picks t_alpha orders from the seeded dataset (each can absorb many 1-cent refunds).
func chaosOrders(seed uint64, n int) []string {
	var out []string
	for _, o := range datagen.Generate(seed).Orders {
		if o.Tenant == datagen.DemoTenant && o.AmountCents >= 5000 && o.OrderID != datagen.DemoOrder {
			out = append(out, o.OrderID)
		}
	}
	sort.Strings(out)
	return out[:min(n, len(out))]
}

type caller struct{ sessions []*mcp.ClientSession }

func (c *caller) call(ctx context.Context, i int, tool string, args map[string]any, meta mcp.Meta) (*mcp.CallToolResult, int64, error) {
	res, err := c.sessions[i%len(c.sessions)].CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args, Meta: meta})
	if err != nil {
		var je *jsonrpc.Error
		if errors.As(err, &je) {
			return nil, je.Code, nil
		}
		return nil, 0, err
	}
	return res, 0, nil
}

// refunded reads an order's refunded amount, retrying while the breaker is open; it returns the
// number of calls it made (each is audited).
func (c *caller) refunded(ctx context.Context, order string) (int64, int64, error) {
	var res *mcp.CallToolResult
	var code int64
	var err error
	calls := int64(0)
	deadline := time.Now().Add(2 * time.Minute)
	for {
		calls++
		res, code, err = c.call(ctx, 0, "orders.get_order", map[string]any{"order_id": order}, nil)
		if err != nil || code != mcperr.CodeUpstreamUnavailable || time.Now().After(deadline) {
			break
		}
		if !pause(ctx, time.Second) {
			break
		}
	}
	if err != nil {
		return 0, calls, fmt.Errorf("get_order %s: %w", order, err)
	}
	if code != 0 {
		return 0, calls, fmt.Errorf("get_order %s: JSON-RPC error %d", order, code)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out struct {
		Refunded int64 `json:"refunded_cents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, calls, fmt.Errorf("get_order %s: %w", order, err)
	}
	return out.Refunded, calls, nil
}

func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Chaos runs the chaos scenario.
func Chaos(ctx context.Context, cfg ChaosConfig) (ChaosResult, error) {
	cfg.defaults()
	agent := "agent-chaos-" + cfg.RunID
	res := ChaosResult{RunID: cfg.RunID, Agent: agent, Calls: cfg.Calls}
	tok, err := devissuer.RequestToken(ctx, cfg.HTTP, cfg.IssuerURL, datagen.DemoTenant, agent, []string{"orders:read", "orders:write"})
	if err != nil {
		return res, fmt.Errorf("agent token: %w", err)
	}
	reviewer, err := devissuer.RequestToken(ctx, cfg.HTTP, cfg.IssuerURL, "*", "sam.security", []string{"mcpgw:audit:read"})
	if err != nil {
		return res, fmt.Errorf("reviewer token: %w", err)
	}
	c := &caller{}
	for range cfg.Sessions {
		s, err := demo.Connect(ctx, cfg.GatewayURL, tok)
		if err != nil {
			return res, err
		}
		defer func() { _ = s.Close() }()
		c.sessions = append(c.sessions, s)
	}
	orders := chaosOrders(cfg.Seed, cfg.Orders)
	if len(orders) == 0 {
		return res, errors.New("no orders to refund")
	}
	before := map[string]int64{}
	var setupCalls int64
	for _, o := range orders {
		v, n, err := c.refunded(ctx, o)
		setupCalls += n
		if err != nil {
			return res, err
		}
		before[o] = v
	}

	var attempts, retries, dups, timeouts, fastFails, inProgress, other, ok, replayed atomic.Int64
	var mu sync.Mutex
	var latencies []float64
	var firstOther string
	keysPerOrder := map[string]int64{}
	for i := range cfg.Calls {
		keysPerOrder[orders[i%len(orders)]]++
	}
	send := func(i int, key string, order string) {
		backoff := 200 * time.Millisecond
		deadline := time.Now().Add(cfg.KeyBudget)
		for a := 0; ; a++ {
			if a > 0 {
				retries.Add(1)
			}
			attempts.Add(1)
			start := time.Now()
			r, code, err := c.call(ctx, i, "orders.refund_order",
				map[string]any{"order_id": order, "amount_cents": 1, "reason": "chaos eval"}, mcp.Meta{"idempotency_key": key})
			took := time.Since(start)
			mu.Lock()
			latencies = append(latencies, float64(took.Microseconds())/1000)
			mu.Unlock()
			switch {
			case err == nil && code == 0 && r != nil && !r.IsError:
				ok.Add(1)
				if m, _ := r.Meta["mcpgw"].(map[string]any); m != nil && m["idempotent_replay"] == true {
					replayed.Add(1)
				}
				return
			case code == mcperr.CodeUpstreamUnavailable && took < 250*time.Millisecond:
				fastFails.Add(1)
			case code == mcperr.CodeUpstreamUnavailable:
				timeouts.Add(1)
			case code == mcperr.CodeIdempotencyInProgress:
				inProgress.Add(1)
			default:
				other.Add(1)
				mu.Lock()
				if firstOther == "" {
					firstOther = fmt.Sprintf("key %s: code %d err %v", key, code, err)
				}
				mu.Unlock()
				return
			}
			if time.Now().After(deadline) || !pause(ctx, backoff) {
				return
			}
			backoff = min(2*backoff, 5*time.Second)
		}
	}

	start := time.Now()
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range cfg.Concurrency {
		wg.Go(func() {
			for i := range jobs {
				key := fmt.Sprintf("chaos-%s-%06d", cfg.RunID, i)
				order := orders[i%len(orders)]
				send(i, key, order)
				// A deterministic share of keys is sent again, as a client that lost the response would.
				if float64(i%100) < cfg.RetryShare*100 {
					dups.Add(1)
					send(i, key, order)
				}
			}
		})
	}
	for i := range cfg.Calls {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	res.Duration = time.Since(start)

	// Stalled transactions commit after the stall: wait at least the stall, then poll the orders
	// until two consecutive reads agree. Not settling within the deadline is an error.
	if !pause(ctx, cfg.SettleWait) {
		return res, ctx.Err()
	}
	after := map[string]int64{}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		changed := false
		for _, o := range orders {
			v, n, err := c.refunded(ctx, o)
			setupCalls += n
			if err != nil {
				return res, err
			}
			if after[o] != v {
				changed = true
			}
			after[o] = v
		}
		if !changed {
			break
		}
		if time.Now().After(deadline) {
			return res, errors.New("orders did not settle within 2 minutes after the run")
		}
	}
	for _, o := range orders {
		inc := after[o] - before[o]
		d := max(0, inc-keysPerOrder[o])
		res.Duplicates += d
		res.Lost += max(0, keysPerOrder[o]-inc)
		res.PerOrder = append(res.PerOrder, OrderResult{OrderID: o, Keys: keysPerOrder[o], Increase: inc, Duplicates: d})
	}

	res.Attempts, res.Retries, res.DeliberateDups = attempts.Load(), retries.Load(), dups.Load()
	res.Timeouts, res.FastFails, res.InProgress, res.OtherErrors = timeouts.Load(), fastFails.Load(), inProgress.Load(), other.Load()
	res.Succeeded, res.Replayed, res.FirstOtherError = ok.Load(), replayed.Load(), firstOther
	res.TimeoutShare = float64(res.Timeouts) / float64(max(1, res.Attempts))
	res.Unanswered = int64(cfg.Calls) + res.DeliberateDups - res.Succeeded
	slices.Sort(latencies)
	res.LatencyP50MS, res.LatencyP95MS = pct(latencies, 50), pct(latencies, 95)

	// Audit: verify (forces a flush), then count this agent's events.
	var verify struct {
		OK bool `json:"ok"`
	}
	if err := getJSON(ctx, cfg.HTTP, cfg.AdminURL+"/admin/audit/verify", reviewer, &verify); err != nil {
		return res, err
	}
	res.ChainOK = verify.OK
	n, err := countAudit(ctx, cfg.HTTP, cfg.AdminURL, reviewer, agent)
	if err != nil {
		return res, err
	}
	res.AuditEvents = n
	res.AuditedRatio = float64(n) / float64(res.Attempts+setupCalls)
	_, _ = fmt.Fprintf(cfg.Out, "chaos: %d calls, %d attempts (%d retries, %d deliberate duplicates), %d timeouts, %d breaker rejects, %d in progress, "+
		"%d other errors; duplicates %d, lost %d; audited %d/%d = %.4f; chain ok %v; %s\n",
		res.Calls, res.Attempts, res.Retries, res.DeliberateDups, res.Timeouts, res.FastFails, res.InProgress, res.OtherErrors,
		res.Duplicates, res.Lost, n, res.Attempts+setupCalls, res.AuditedRatio, res.ChainOK, res.Duration.Round(time.Millisecond))
	return res, nil
}

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	k := int(float64(len(sorted))*p/100+0.999999) - 1
	return sorted[max(0, min(k, len(sorted)-1))]
}

func getJSON(ctx context.Context, hc *http.Client, u, token string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("request %s: %w", u, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("get %s: status %d: %s", u, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", u, err)
	}
	return nil
}

func countAudit(ctx context.Context, hc *http.Client, admin, token, agent string) (int64, error) {
	var n int64
	cursor := ""
	for {
		q := url.Values{"agent": {agent}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Events     []json.RawMessage `json:"events"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := getJSON(ctx, hc, admin+"/admin/audit?"+q.Encode(), token, &page); err != nil {
			return n, err
		}
		n += int64(len(page.Events))
		if page.NextCursor == "" {
			return n, nil
		}
		cursor = page.NextCursor
	}
}
