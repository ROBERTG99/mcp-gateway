// Package idempotency stores the state of write calls keyed by (tenant, idempotency key) so a
// retried write executes upstream at most once through the gateway. Backends: memory (tests),
// Redis (default) and DynamoDB, all held to the same contract by storetest.Run.
package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Outcome of an Acquire.
type Outcome int

// Outcomes.
const (
	// Acquired: the caller owns the key and must Complete or Release it.
	Acquired Outcome = iota + 1
	// Replay: the key completed with the same args hash; Result holds the stored result.
	Replay
	// Conflict: the key is used (in flight or completed) with a different args hash (-32040).
	Conflict
	// InProgress: the key is in flight with the same args hash (-32047).
	InProgress
)

func (o Outcome) String() string {
	switch o {
	case Acquired:
		return "acquired"
	case Replay:
		return "replay"
	case Conflict:
		return "conflict"
	case InProgress:
		return "in_progress"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// Acquisition is the result of Acquire.
type Acquisition struct {
	Outcome Outcome
	Result  json.RawMessage // set for Replay
}

// ErrNotOwner is returned by Complete when the key is not in flight with that args hash.
var ErrNotOwner = errors.New("idempotency key not held with this args hash")

// Store is an idempotency backend.
type Store interface {
	// Acquire claims (tenant, key) for argsHash with an in-progress lock lasting lockTTL.
	Acquire(ctx context.Context, tenant, key, argsHash string, lockTTL time.Duration) (Acquisition, error)
	// Complete records the result; the record lives for ttl.
	Complete(ctx context.Context, tenant, key, argsHash string, result json.RawMessage, ttl time.Duration) error
	// Release drops an in-progress claim (after a failed or timed-out call) so a retry can
	// proceed. Completed records are never released.
	Release(ctx context.Context, tenant, key, argsHash string) error
	// Ping checks the backend.
	Ping(ctx context.Context) error
}

const (
	stateInProgress = "in_progress"
	stateCompleted  = "completed"
)

// record is the stored value shared by the backends.
type record struct {
	State    string          `json:"state"`
	ArgsHash string          `json:"args_sha256"`
	Result   json.RawMessage `json:"result,omitempty"`
}

// decide maps an existing record to an outcome for a caller with argsHash.
func decide(r record, argsHash string) Acquisition {
	switch {
	case r.ArgsHash != argsHash:
		return Acquisition{Outcome: Conflict}
	case r.State == stateCompleted:
		return Acquisition{Outcome: Replay, Result: r.Result}
	default:
		return Acquisition{Outcome: InProgress}
	}
}
