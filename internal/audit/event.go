// Package audit records one mcpgw.audit.v1 event per tools/call outcome in a SHA-256 hash chain:
// hash = sha256(prev_hash + canonical_json(event without hash)), genesis prev_hash = 64 zeros.
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
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

// MarshalJSON serves ts in the same fixed microsecond layout the hash covers, so any consumer can
// re-hash an event from its JSON (time.Time's default drops trailing zeros).
func (e Event) MarshalJSON() ([]byte, error) {
	type plain Event
	return json.Marshal(struct {
		plain
		TS string `json:"ts"`
	}{plain: plain(e), TS: e.TS.UTC().Format(tsLayout)})
}

// ErrUnstorable marks a sink error caused by the events themselves (a value the store rejects), as
// opposed to a transient failure: retrying the same batch can never succeed.
var ErrUnstorable = errors.New("audit event unstorable")

// Markers replace values that cannot be stored or hashed.
const (
	nulMarker        = "[REDACTED:NUL]"
	numberMarker     = "[REDACTED:NUMBER]"
	invalidMarker    = `"[REDACTED:INVALID_JSON]"`
	unstorableMarker = `"[REDACTED:UNSTORABLE]"`
)

// Normalize makes the event survive a Postgres round trip unchanged and always storable: UTC
// microsecond timestamps, finite latency rounded to microseconds, strings without NUL or invalid
// UTF-8, and canonical I-JSON args (unrepresentable numbers and NULs replaced by markers).
func (e *Event) Normalize() {
	e.TS = e.TS.UTC().Truncate(time.Microsecond)
	if math.IsNaN(e.LatencyMS) || math.IsInf(e.LatencyMS, 0) {
		e.LatencyMS = 0
	}
	e.LatencyMS = math.Round(e.LatencyMS*1000) / 1000
	for _, s := range []*string{&e.Tenant, &e.Agent, &e.Server, &e.Tool, &e.ArgsSHA256, &e.Decision, &e.Status} {
		*s = cleanString(*s)
	}
	for _, s := range []*string{e.ApprovalID, e.IdempotencyKey} {
		if s != nil {
			*s = cleanString(*s)
		}
	}
	e.ArgsRedacted = storableArgs(e.ArgsRedacted)
}

// placeholder stands in for an event the sink refused: same identity and outcome, no args.
func (e Event) placeholder() Event {
	e.ArgsRedacted = json.RawMessage(unstorableMarker)
	return e
}

func cleanString(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", nulMarker)
}

// storableArgs returns the canonical form of raw, sanitising it first when it is not I-JSON or
// holds a NUL (Postgres jsonb rejects \u0000).
func storableArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	if c, err := canonicaljson.Canonicalize(raw); err == nil && !bytes.Contains(c, []byte(`\u0000`)) {
		return c
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return json.RawMessage(invalidMarker)
	}
	b, err := json.Marshal(sanitize(v))
	if err != nil {
		return json.RawMessage(invalidMarker)
	}
	c, err := canonicaljson.Canonicalize(b)
	if err != nil {
		return json.RawMessage(invalidMarker)
	}
	return c
}

func sanitize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[cleanString(k)] = sanitize(x)
		}
		return out
	case []any:
		for i := range t {
			t[i] = sanitize(t[i])
		}
		return t
	case string:
		return cleanString(t)
	case json.Number:
		if _, err := canonicaljson.Canonicalize([]byte(t)); err != nil {
			return numberMarker
		}
		return t
	default:
		return v
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
		return nil, fmt.Errorf("%w: canonicalize audit event %d: %w", ErrUnstorable, e.Seq, err)
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
