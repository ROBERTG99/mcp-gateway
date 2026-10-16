package datagen_test

import (
	"strings"
	"testing"

	"github.com/ROBERTG99/mcp-gateway/internal/datagen"
)

func TestDatagenDeterministic(t *testing.T) {
	a := datagen.Generate(datagen.DefaultSeed)
	b := datagen.Generate(datagen.DefaultSeed)
	if a.Hash() != b.Hash() {
		t.Fatalf("hash differs across runs: %s vs %s", a.Hash(), b.Hash())
	}
	if c := datagen.Generate(datagen.DefaultSeed + 1); c.Hash() == a.Hash() {
		t.Fatal("different seeds produced the same dataset")
	}
	if a.Schema != "datagen.v1" || a.Seed != 20260 {
		t.Fatalf("schema/seed = %s/%d", a.Schema, a.Seed)
	}
	if got := len(a.Customers); got != 600 {
		t.Errorf("customers = %d, want 600", got)
	}
	if got := len(a.Orders); got != 2000 {
		t.Errorf("orders = %d, want 2000", got)
	}
	if got := len(a.Notes); got != 1500 {
		t.Errorf("notes = %d, want 1500", got)
	}
	injected := 0
	for _, n := range a.Notes {
		if n.Injected {
			injected++
		}
	}
	if injected != 40 {
		t.Errorf("injected notes = %d, want 40", injected)
	}
	tenants := map[string]int{}
	for _, c := range a.Customers {
		tenants[c.Tenant]++
		if !strings.HasSuffix(c.Email, "@example.com") {
			t.Fatalf("non-synthetic email %q", c.Email)
		}
	}
	for _, id := range []string{"t_alpha", "t_beta", "t_gamma"} {
		if tenants[id] != 200 {
			t.Errorf("tenant %s has %d customers, want 200", id, tenants[id])
		}
	}
}

func TestDemoCustomerHasInjectedLatestNote(t *testing.T) {
	d := datagen.Generate(datagen.DefaultSeed)
	var latest *datagen.Note
	for i := range d.Notes {
		n := &d.Notes[i]
		if n.Tenant == datagen.DemoTenant && n.CustomerID == datagen.DemoInjectedCustomer {
			if latest == nil || n.CreatedAt.After(latest.CreatedAt) {
				latest = n
			}
		}
	}
	if latest == nil || !latest.Injected || !strings.Contains(strings.ToLower(latest.Body), "ignore previous instructions") {
		t.Fatalf("demo customer latest note = %+v", latest)
	}
}
