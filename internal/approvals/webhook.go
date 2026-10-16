package approvals

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
)

// Notifier delivers an approval webhook payload.
type Notifier interface {
	Deliver(ctx context.Context, payload []byte) error
}

// WebhookNotifier POSTs payloads signed with X-MCPGW-Signature: sha256=<hex HMAC-SHA256>.
type WebhookNotifier struct {
	url    string
	secret []byte
	client *http.Client
}

// NewWebhookNotifier returns a notifier (client nil = 10 s timeout default client).
func NewWebhookNotifier(url, secret string, client *http.Client) *WebhookNotifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &WebhookNotifier{url: url, secret: []byte(secret), client: client}
}

// Sign returns the X-MCPGW-Signature header value for body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks a signature header in constant time (for webhook receivers).
func VerifySignature(secret string, body []byte, header string) bool {
	return hmac.Equal([]byte(Sign(secret, body)), []byte(header))
}

// Deliver implements Notifier; any non-2xx status is a failure.
func (n *WebhookNotifier) Deliver(ctx context.Context, payload []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-MCPGW-Signature", Sign(string(n.secret), payload))
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook post: status %d", resp.StatusCode)
	}
	return nil
}

// OutboxOptions configure delivery retries (spec §4.6: 2 s doubling to 1 h, 24 attempts).
type OutboxOptions struct {
	Clock       clock.Clock
	Logger      *slog.Logger
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	MaxAttempts int
	BatchSize   int
}

// OutboxWorker delivers due webhook outbox rows.
type OutboxWorker struct {
	store    Store
	notifier Notifier
	opts     OutboxOptions
}

// NewOutboxWorker returns a worker.
func NewOutboxWorker(store Store, notifier Notifier, opts OutboxOptions) *OutboxWorker {
	if opts.Clock == nil {
		opts.Clock = clock.Real{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.BaseDelay == 0 {
		opts.BaseDelay = 2 * time.Second
	}
	if opts.MaxDelay == 0 {
		opts.MaxDelay = time.Hour
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 24
	}
	if opts.BatchSize == 0 {
		opts.BatchSize = 50
	}
	return &OutboxWorker{store: store, notifier: notifier, opts: opts}
}

// Backoff is the delay after the n-th failed attempt (1-based).
func (w *OutboxWorker) Backoff(n int) time.Duration {
	d := w.opts.BaseDelay
	for i := 1; i < n && d < w.opts.MaxDelay; i++ {
		d *= 2
	}
	return min(d, w.opts.MaxDelay)
}

// RunOnce delivers every due item once.
func (w *OutboxWorker) RunOnce(ctx context.Context) (delivered, failed int, err error) {
	now := w.opts.Clock.Now().UTC()
	items, err := w.store.DueOutbox(ctx, now, w.opts.BatchSize)
	if err != nil {
		return 0, 0, err
	}
	for _, it := range items {
		if derr := w.notifier.Deliver(ctx, it.Payload); derr == nil {
			if err := w.store.MarkDelivered(ctx, it.ID, w.opts.Clock.Now().UTC()); err != nil {
				return delivered, failed, err
			}
			delivered++
			continue
		} else {
			attempts := it.Attempts + 1
			next := now.Add(w.Backoff(attempts))
			msg := derr.Error()
			if attempts >= w.opts.MaxAttempts {
				next = time.Time{} // give up: never retried, kept for the runbook
				msg = fmt.Sprintf("gave up after %d attempts: %s", attempts, msg)
				w.opts.Logger.Error("approval webhook abandoned", "approval_id", it.ApprovalID, "attempts", attempts)
			}
			if err := w.store.MarkFailed(ctx, it.ID, attempts, next, msg); err != nil {
				return delivered, failed, err
			}
			failed++
		}
	}
	return delivered, failed, nil
}

// Run polls every interval until ctx is cancelled.
func (w *OutboxWorker) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, _, err := w.RunOnce(ctx); err != nil {
				w.opts.Logger.Warn("webhook outbox run failed", "error", err)
			}
		}
	}
}
