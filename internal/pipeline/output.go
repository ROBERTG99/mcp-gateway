package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ROBERTG99/mcp-gateway/internal/injection"
)

// UntrustedPrefix is the first line of every text block returned by the gateway.
const UntrustedPrefix = "<<untrusted tool output>>\n"

// Scorer scores text for prompt-injection signals (injection.Scorer).
type Scorer interface {
	Score(text string) injection.Result
}

// Finding is a flagged upstream output (injection_findings).
type Finding struct {
	AuditSeq int64
	Tenant   string
	Server   string
	Tool     string
	Score    float64
	Signals  []string
	TS       time.Time
}

// FindingSink stores findings.
type FindingSink interface {
	RecordFinding(ctx context.Context, f Finding) error
}

// mark applies the untrusted-output contract to a result (a copy; the input is not modified):
// structuredContent.untrusted = true (object created if absent), text blocks prefixed, and the
// injection score in _meta.mcpgw.injection. In block mode, flagged text is withheld.
func (p *Pipeline) mark(ctx context.Context, c *call, res *mcp.CallToolResult) (*mcp.CallToolResult, injection.Result) {
	score := injection.Result{Signals: []string{}}
	if p.deps.Scorer != nil {
		score = p.deps.Scorer.Score(outputText(res))
	}
	flagged := p.deps.Scorer != nil && score.Score >= p.opts.InjectionThreshold
	out := *res
	out.Content = make([]mcp.Content, 0, len(res.Content))
	withhold := flagged && p.opts.InjectionMode == "block"
	for _, ct := range res.Content {
		tc, ok := ct.(*mcp.TextContent)
		if !ok {
			out.Content = append(out.Content, ct)
			continue
		}
		text := tc.Text
		if withhold {
			text = fmt.Sprintf("[withheld by mcp-gateway: possible prompt injection, score %.2f, signals %s]",
				score.Score, strings.Join(score.Signals, ","))
		}
		cp := *tc
		cp.Text = UntrustedPrefix + text
		out.Content = append(out.Content, &cp)
	}
	switch sc := res.StructuredContent.(type) {
	case nil:
		out.StructuredContent = map[string]any{"untrusted": true}
	case map[string]any:
		m := make(map[string]any, len(sc)+1)
		for k, v := range sc {
			m[k] = v
		}
		m["untrusted"] = true
		out.StructuredContent = m
	default:
		out.StructuredContent = map[string]any{"untrusted": true, "value": sc}
	}
	if withhold {
		out.StructuredContent = map[string]any{"untrusted": true, "withheld": true}
	}
	if flagged && p.deps.Findings != nil {
		f := Finding{AuditSeq: c.auditSeq, Tenant: c.principal.Tenant, Server: c.tool.Server, Tool: c.tool.Name,
			Score: score.Score, Signals: score.Signals, TS: p.deps.Clock.Now().UTC()}
		if err := p.deps.Findings.RecordFinding(context.WithoutCancel(ctx), f); err != nil {
			p.deps.Logger.Warn("record injection finding failed", "request_id", c.requestID, "error", err)
		}
	}
	return &out, score
}

// outputText collects the text the agent will read: text blocks and structured string values.
func outputText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
			b.WriteByte('\n')
		}
	}
	collectStrings(res.StructuredContent, &b)
	return b.String()
}

func collectStrings(v any, b *strings.Builder) {
	switch x := v.(type) {
	case string:
		b.WriteString(x)
		b.WriteByte('\n')
	case map[string]any:
		for _, val := range x {
			collectStrings(val, b)
		}
	case []any:
		for _, val := range x {
			collectStrings(val, b)
		}
	}
}
