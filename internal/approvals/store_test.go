package approvals_test

import (
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/approvals"
	"github.com/ROBERTG99/mcp-gateway/internal/approvals/approvalstest"
)

func TestMemoryStore(t *testing.T) {
	approvalstest.Run(t, func(*testing.T) approvals.Store { return approvals.NewMemory() })
}
