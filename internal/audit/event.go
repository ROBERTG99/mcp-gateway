// Package audit records one mcpgw.audit.v1 event per tools/call outcome in a SHA-256 hash chain:
// hash = sha256(prev_hash + canonical_json(event without hash)), genesis prev_hash = 64 zeros.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/ROBERTG99/mcp-gateway/pkg/canonicaljson"
)

// Contract constants.
const (
	Schema      = "mcpgw.audit.v1"
	GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"
	tsLayout    = "2006-01-02T15:04:05.000000Z"
)

// Event is one audit record.
type Event struct {
	Seq            int64           `json:"seq"`
	ChainPos       int64           `json:"-"`
	TS             time.Time       `json:"ts"`
	Tenant         string          `json:"tenant"`
	Agent          string          `json:"agent"`
	Server         string          `json:"server"`
	Tool           string          `json:"tool"`
	ArgsRedacted   json.RawMessage `json:"args_redacted"`
	ArgsSHA256     string          `json:"args_sha256"`
	Decision       string          `json:"decision"`
	ApprovalID     *string         `json:"approval_id"`
	IdempotencyKey *string         `json:"idempotency_key"`
	Status         string          `json:"status"`
	LatencyMS      float64         `json:"latency_ms"`
	PrevHash       string          `json:"prev_hash"`
	Hash           string          `json:"hash"`
}

// Normalize makes the event survive a Postgres round trip unchanged: UTC microsecond timestamps,
// latency rounded to microseconds, and canonical args JSON.
func (e *Event) Normalize() {
	e.TS = e.TS.UTC().Truncate(time.Microsecond)
	e.LatencyMS = math.Round(e.LatencyMS*1000) / 1000
	if len(e.ArgsRedacted) == 0 {
		e.ArgsRedacted = json.RawMessage("null")
	}
	if c, err := canonicaljson.Canonicalize(e.ArgsRedacted); err == nil {
		e.ArgsRedacted = c
	}
}

// body is the canonical JSON of the event without its hash.
func (e Event) body() ([]byte, error) {
	m := map[string]any{
		"seq": e.Seq, "ts": e.TS.UTC().Format(tsLayout), "tenant": e.Tenant, "agent": e.Agent, "server": e.Server,
		"tool": e.Tool, "args_redacted": e.ArgsRedacted, "args_sha256": e.ArgsSHA256, "decision": e.Decision,
		"approval_id": e.ApprovalID, "idempotency_key": e.IdempotencyKey, "status": e.Status,
		"latency_ms": e.LatencyMS, "prev_hash": e.PrevHash,
	}
	b, err := canonicaljson.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("canonicalize audit event %d: %w", e.Seq, err)
	}
	return b, nil
}

// ComputeHash returns sha256(prev_hash + canonical_json(event without hash)) in hex.
func (e Event) ComputeHash() (string, error) {
	b, err := e.body()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte(e.PrevHash))
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyResult is the outcome of a chain verification.
type VerifyResult struct {
	OK          bool   `json:"ok"`
	Checked     int    `json:"checked"`
	FirstBadSeq int64  `json:"first_bad_seq,omitempty"`
	Reason      string `json:"reason,omitempty"`
	HeadHash    string `json:"head_hash,omitempty"`
}

// VerifyChain checks events (in chain order). anchor is the expected prev_hash of the first
// event: GenesisHash for a full chain, or "" to trust the first event's prev_hash (--from-seq).
func VerifyChain(events []Event, anchor string) VerifyResult {
	prev := anchor
	for i, e := range events {
		if i == 0 && anchor == "" {
			prev = e.PrevHash
		}
		if e.PrevHash != prev {
			return VerifyResult{Checked: i, FirstBadSeq: e.Seq, Reason: "prev_hash does not match the previous event's hash"}
		}
		h, err := e.ComputeHash()
		if err != nil {
			return VerifyResult{Checked: i, FirstBadSeq: e.Seq, Reason: err.Error()}
		}
		if h != e.Hash {
			return VerifyResult{Checked: i, FirstBadSeq: e.Seq, Reason: "hash does not match the event content"}
		}
		prev = e.Hash
	}
	return VerifyResult{OK: true, Checked: len(events), HeadHash: prev}
}

// chain assigns chain positions and hashes to events following (headPos, headHash).
func chain(events []Event, headPos int64, headHash string) error {
	for i := range events {
		events[i].ChainPos = headPos + int64(i) + 1
		events[i].PrevHash = headHash
		h, err := events[i].ComputeHash()
		if err != nil {
			return err
		}
		events[i].Hash = h
		headHash = h
	}
	return nil
}
