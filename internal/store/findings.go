package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ROBERTG99/mcp-gateway/internal/pipeline"
)

// Findings stores injection findings in injection_findings.
type Findings struct {
	pool *pgxpool.Pool
}

// NewFindings returns the findings store.
func NewFindings(pool *pgxpool.Pool) *Findings { return &Findings{pool: pool} }

// RecordFinding implements pipeline.FindingSink.
func (f *Findings) RecordFinding(ctx context.Context, fd pipeline.Finding) error {
	var seq any
	if fd.AuditSeq > 0 {
		seq = fd.AuditSeq
	}
	_, err := f.pool.Exec(ctx, `INSERT INTO injection_findings (audit_seq, tenant, server, tool, score, signals, ts)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, seq, fd.Tenant, fd.Server, fd.Tool, fd.Score, fd.Signals, fd.TS)
	if err != nil {
		return fmt.Errorf("insert finding: %w", err)
	}
	return nil
}

// FindingRow is a stored finding.
type FindingRow struct {
	ID       int64
	AuditSeq *int64
	Tenant   string
	Server   string
	Tool     string
	Score    float32
	Signals  []string
}

// ListFindings returns the latest findings of a tenant ("" = all), newest first.
func (f *Findings) ListFindings(ctx context.Context, tenant string, limit int) ([]FindingRow, error) {
	rows, err := f.pool.Query(ctx, `SELECT id, audit_seq, tenant, server, tool, score, signals FROM injection_findings
		WHERE $1 = '' OR tenant = $1 ORDER BY ts DESC, id DESC LIMIT $2`, tenant, limit)
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	defer rows.Close()
	var out []FindingRow
	for rows.Next() {
		var r FindingRow
		if err := rows.Scan(&r.ID, &r.AuditSeq, &r.Tenant, &r.Server, &r.Tool, &r.Score, &r.Signals); err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
