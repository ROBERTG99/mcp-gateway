package loadgen

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/devissuer"
)

// LoadConfig configures a load test (NFR-2 throughput, NFR-7 memory).
type LoadConfig struct {
	GatewayURL, IssuerURL string
	MetricsURL            string        // gateway Prometheus endpoint, sampled for process_resident_memory_bytes ("" skips RSS)
	Duration              time.Duration // measured period (default 30 s)
	Warmup                time.Duration // unmeasured period before it (default 3 s)
	Concurrency           int           // parallel workers (default 64)
	Sessions              int           // MCP sessions shared by the workers (default 16)
	WriteShare            float64       // share of calls that are refunds with fresh keys (default 0.2)
	RunID                 string
	HTTP                  *http.Client // admin and metrics requests
	Out                   io.Writer
	Seed                  uint64 // datagen seed of the seeded stack
}

func (c *LoadConfig) defaults() {
	if c.Duration <= 0 {
		c.Duration = 30 * time.Second
	}
	if c.Warmup < 0 {
		c.Warmup = 0
	} else if c.Warmup == 0 {
		c.Warmup = 3 * time.Second
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 64
	}
	if c.Sessions <= 0 {
		c.Sessions = 16
	}
	if c.WriteShare == 0 {
		c.WriteShare = 0.2
	}
	if c.RunID == "" {
		c.RunID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if c.Out == nil {
		c.Out = io.Discard
	}
	if c.Seed == 0 {
		c.Seed = datagen.DefaultSeed
	}
}

// LoadResult is the outcome of a load test; the JSON form is what scripts/bench_budget.py --load checks.
type LoadResult struct {
	RunID          string  `json:"run_id"`
	Agent          string  `json:"agent"`
	Concurrency    int     `json:"concurrency"`
	DurationS      float64 `json:"duration_s"`
	Calls          int64   `json:"calls"`
	Reads          int64   `json:"reads"`
	Writes         int64   `json:"writes"`
	Errors         int64   `json:"errors"`
	ErrorRatio     float64 `json:"error_ratio"`
	CallsPerSecond float64 `json:"calls_per_second"`
	LatencyP50MS   float64 `json:"latency_p50_ms"`
	LatencyP95MS   float64 `json:"latency_p95_ms"`
	LatencyP99MS   float64 `json:"latency_p99_ms"`
	RSSMaxMB       float64 `json:"rss_max_mb"`
	RSSSamples     int     `json:"rss_samples"`
	FirstError     string  `json:"first_error,omitempty"`
}

// bearerTransport adds the agent token; its pool keeps one idle connection per worker.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r) //nolint:wrapcheck // transport passthrough
}

// Load runs the load test: workers call the gateway as fast as it answers for Warmup plus
// Duration, 80% reads (crm.get_customer, orders.get_order) and 20% refunds of 1 cent with fresh
// idempotency keys, all for one t_alpha agent. Only calls that start inside the measured period
// count; RSS is the largest gateway sample during it.
func Load(ctx context.Context, cfg LoadConfig) (LoadResult, error) {
	cfg.defaults()
	agent := "agent-load-" + cfg.RunID
	res := LoadResult{RunID: cfg.RunID, Agent: agent, Concurrency: cfg.Concurrency}
	tok, err := devissuer.RequestToken(ctx, cfg.HTTP, cfg.IssuerURL, datagen.DemoTenant, agent, []string{"crm:read", "orders:read", "orders:write"})
	if err != nil {
		return res, fmt.Errorf("agent token: %w", err)
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.MaxIdleConns, base.MaxIdleConnsPerHost = cfg.Concurrency*2, cfg.Concurrency*2
	hc := &http.Client{Transport: bearerTransport{token: tok, next: base}}
	c := &caller{}
	for range cfg.Sessions {
		s, err := mcp.NewClient(&mcp.Implementation{Name: "loadgen", Version: "v0.1.0"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: cfg.GatewayURL, HTTPClient: hc}, nil)
		if err != nil {
			return res, fmt.Errorf("connect %s: %w", cfg.GatewayURL, err)
		}
		defer func() { _ = s.Close() }()
		c.sessions = append(c.sessions, s)
	}
	orders := chaosOrders(cfg.Seed, 1<<30)
	if len(orders) == 0 {
		return res, errors.New("no orders in the seeded dataset")
	}

	start := time.Now()
	measureFrom, end := start.Add(cfg.Warmup), start.Add(cfg.Warmup+cfg.Duration)
	runCtx, cancel := context.WithDeadline(ctx, end.Add(30*time.Second))
	defer cancel()

	var rssMax atomic.Int64
	var rssSamples atomic.Int64
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		if cfg.MetricsURL == "" {
			return
		}
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case now := <-t.C:
				if now.After(end) {
					return
				}
				if now.Before(measureFrom) {
					continue
				}
				if v, err := residentBytes(runCtx, cfg.HTTP, cfg.MetricsURL); err == nil {
					rssSamples.Add(1)
					for cur := rssMax.Load(); v > cur && !rssMax.CompareAndSwap(cur, v); cur = rssMax.Load() {
					}
				}
			}
		}
	}()

	var reads, writes, errs atomic.Int64
	var mu sync.Mutex
	var latencies []float64
	var firstErr string
	var seq atomic.Int64
	var wg sync.WaitGroup
	for w := range cfg.Concurrency {
		wg.Go(func() {
			rng := rand.New(rand.NewPCG(cfg.Seed, uint64(w))) //nolint:gosec // load mix, not security
			local := make([]float64, 0, 4096)
			for {
				began := time.Now()
				if began.After(end) || runCtx.Err() != nil {
					break
				}
				i := int(seq.Add(1))
				var tool string
				var args map[string]any
				var meta mcp.Meta
				write := rng.Float64() < cfg.WriteShare
				switch {
				case write:
					tool, args = "orders.refund_order", map[string]any{"order_id": orders[i%len(orders)], "amount_cents": 1, "reason": "load test"}
					meta = mcp.Meta{"idempotency_key": fmt.Sprintf("load-%s-%09d", cfg.RunID, i)}
				case i%2 == 0:
					tool, args = "crm.get_customer", map[string]any{"customer_id": fmt.Sprintf("C-%04d", 1+i%datagen.CustomersPerTenant)}
				default:
					tool, args = "orders.get_order", map[string]any{"order_id": orders[i%len(orders)]}
				}
				r, code, err := c.call(runCtx, i, tool, args, meta)
				took := time.Since(began)
				if began.Before(measureFrom) {
					continue
				}
				local = append(local, float64(took.Microseconds())/1000)
				if write {
					writes.Add(1)
				} else {
					reads.Add(1)
				}
				if err != nil || code != 0 || r == nil || r.IsError {
					errs.Add(1)
					mu.Lock()
					if firstErr == "" {
						firstErr = fmt.Sprintf("%s: code %d err %v", tool, code, err)
					}
					mu.Unlock()
				}
			}
			mu.Lock()
			latencies = append(latencies, local...)
			mu.Unlock()
		})
	}
	wg.Wait()
	cancel()
	<-samplerDone

	elapsed := time.Since(measureFrom)
	res.DurationS = elapsed.Seconds()
	res.Reads, res.Writes, res.Errors = reads.Load(), writes.Load(), errs.Load()
	res.Calls = res.Reads + res.Writes
	res.ErrorRatio = float64(res.Errors) / float64(max(1, res.Calls))
	res.CallsPerSecond = float64(res.Calls) / res.DurationS
	slices.Sort(latencies)
	res.LatencyP50MS, res.LatencyP95MS, res.LatencyP99MS = pct(latencies, 50), pct(latencies, 95), pct(latencies, 99)
	res.RSSMaxMB = float64(rssMax.Load()) / (1 << 20)
	res.RSSSamples = int(rssSamples.Load())
	res.FirstError = firstErr
	_, _ = fmt.Fprintf(cfg.Out, "load: %d calls in %.1f s = %.0f calls/s (%d reads, %d writes, %d errors), latency p50 %.2f ms p95 %.2f ms p99 %.2f ms, "+
		"gateway RSS max %.1f MB (%d samples), concurrency %d\n", res.Calls, res.DurationS, res.CallsPerSecond, res.Reads, res.Writes, res.Errors,
		res.LatencyP50MS, res.LatencyP95MS, res.LatencyP99MS, res.RSSMaxMB, res.RSSSamples, res.Concurrency)
	if cfg.MetricsURL != "" && res.RSSSamples == 0 {
		return res, fmt.Errorf("no RSS sample from %s", cfg.MetricsURL)
	}
	return res, nil
}

// residentBytes reads process_resident_memory_bytes from a Prometheus text endpoint.
func residentBytes(ctx context.Context, hc *http.Client, u string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("request %s: %w", u, err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("get %s: %w", u, err)
	}
	defer func() { _ = resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "process_resident_memory_bytes "); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return 0, fmt.Errorf("parse rss %q: %w", v, err)
			}
			return int64(f), nil
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", u, err)
	}
	return 0, fmt.Errorf("%s: no process_resident_memory_bytes", u)
}
