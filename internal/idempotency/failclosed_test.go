package idempotency_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/redis/go-redis/v9"

	"github.com/ROBERTG99/mcp-gateway/internal/clock"
	"github.com/ROBERTG99/mcp-gateway/internal/idempotency"
)

// TestStoresUnreachableFailClosed (NFR-3): when the backing store cannot be reached, every
// operation returns an error, never an outcome, so the pipeline refuses the write (-32049).
func TestStoresUnreachableFailClosed(t *testing.T) {
	c := clock.NewFake(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC))
	rc := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	t.Cleanup(func() { _ = rc.Close() })
	dc := dynamodb.New(dynamodb.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://127.0.0.1:1"),
		Credentials: credentials.NewStaticCredentialsProvider("local", "local", ""), RetryMaxAttempts: 1})
	stores := map[string]idempotency.Store{
		"redis":    idempotency.NewRedis(rc),
		"dynamodb": idempotency.NewDynamoDB(dc, "mcpgw_idempotency", c),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for name, s := range stores {
		if _, err := s.Acquire(ctx, "t_alpha", "key-00000001", "h", time.Minute); err == nil {
			t.Errorf("%s: Acquire succeeded against an unreachable store", name)
		}
		if err := s.Complete(ctx, "t_alpha", "key-00000001", "h", json.RawMessage(`{}`), time.Hour); err == nil {
			t.Errorf("%s: Complete succeeded against an unreachable store", name)
		}
		if err := s.Release(ctx, "t_alpha", "key-00000001", "h"); err == nil {
			t.Errorf("%s: Release succeeded against an unreachable store", name)
		}
		if err := s.Ping(ctx); err == nil {
			t.Errorf("%s: Ping succeeded against an unreachable store", name)
		}
	}
	if err := stores["dynamodb"].(*idempotency.DynamoDB).EnsureTable(ctx); err == nil {
		t.Error("EnsureTable succeeded against an unreachable endpoint")
	}
}

func TestOpenSelectsBackend(t *testing.T) {
	ctx := t.Context()
	c := clock.NewFake(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC))
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	for _, tc := range []struct {
		opts    idempotency.OpenOptions
		want    string
		wantErr bool
	}{
		{idempotency.OpenOptions{Backend: "memory", Clock: c}, "*idempotency.Memory", false},
		{idempotency.OpenOptions{Backend: "redis", Redis: rc, Clock: c}, "*idempotency.Redis", false},
		{idempotency.OpenOptions{Backend: "redis", Clock: c}, "", true},
		{idempotency.OpenOptions{Backend: "memcached", Clock: c}, "", true},
		// No endpoint: the default AWS credential chain, and the table is not created.
		{idempotency.OpenOptions{Backend: "dynamodb", DynamoDBTable: "t", DynamoDBRegion: "eu-west-1", Clock: c}, "*idempotency.DynamoDB", false},
	} {
		s, err := idempotency.Open(ctx, tc.opts)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Open(%s): no error", tc.opts.Backend)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Open(%s): %v", tc.opts.Backend, err)
		}
		if got := fmt.Sprintf("%T", s); got != tc.want {
			t.Errorf("Open(%s) = %s, want %s", tc.opts.Backend, got, tc.want)
		}
	}
}

func TestOutcomeString(t *testing.T) {
	for o, want := range map[idempotency.Outcome]string{idempotency.Acquired: "acquired", idempotency.Replay: "replay",
		idempotency.Conflict: "conflict", idempotency.InProgress: "in_progress", idempotency.Outcome(99): "outcome(99)"} {
		if got := o.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(o), got, want)
		}
	}
}
