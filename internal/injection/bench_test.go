package injection_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
	"github.com/ROBERTG99/mcp-gateway/internal/injection"
)

// BenchmarkScoreToolResult scores what the gateway scores for one crm.get_customer result of the
// seeded dataset: the JSON text content followed by every string of the structured content.
func BenchmarkScoreToolResult(b *testing.B) {
	d := datagen.Generate(datagen.DefaultSeed)
	c := d.Customers[0]
	out := map[string]any{"customer_id": c.CustomerID, "name": c.Name, "email": c.Email, "phone": c.Phone, "segment": c.Segment}
	var notes []any
	for _, n := range d.Notes {
		if n.Tenant == c.Tenant && n.CustomerID == c.CustomerID && len(notes) < 5 {
			notes = append(notes, map[string]any{"body": n.Body, "author": n.Author, "created_at": n.CreatedAt.Format("2006-01-02T15:04:05Z")})
		}
	}
	out["notes"] = notes
	raw, _ := json.Marshal(out)
	var sb strings.Builder
	sb.Write(raw)
	sb.WriteByte('\n')
	for _, k := range []string{"customer_id", "name", "email", "phone", "segment"} {
		sb.WriteString(out[k].(string) + "\n")
	}
	for _, n := range notes {
		for _, v := range n.(map[string]any) {
			sb.WriteString(v.(string) + "\n")
		}
	}
	text := sb.String()
	s := injection.New(injection.Options{})
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		s.Score(text)
	}
}
