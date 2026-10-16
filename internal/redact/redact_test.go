package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ROBERTG99/mcp-gateway/internal/redact"
)

func TestRedactorDetectors(t *testing.T) {
	r := redact.New()
	cases := []struct{ in, want string }{
		{"card 4111 1111 1111 1111 on file", "card [REDACTED:CARD] on file"},
		{"card 4111-1111-1111-1111", "card [REDACTED:CARD]"},
		{"card 4111111111111111.", "card [REDACTED:CARD]."},
		{"card 4111 1111 1111 1112 fails Luhn", "card 4111 1111 1111 1112 fails Luhn"},
		{"amex 3782 822463 10005", "amex [REDACTED:CARD]"},
		{"mail jo.approver+ops@example.com now", "mail [REDACTED:EMAIL] now"},
		{"iban GB82 WEST 1234 5698 7654 32", "iban [REDACTED:IBAN]"},
		{"iban DE89370400440532013000", "iban [REDACTED:IBAN]"},
		{"iban GB82 WEST 1234 5698 7654 33 is wrong", "iban GB82 WEST 1234 5698 7654 33 is wrong"},
		{"call +1 202 555 0143 today", "call [REDACTED:PHONE] today"},
		{"call (202) 555-0143", "call [REDACTED:PHONE]"},
		{"call 202-555-0143", "call [REDACTED:PHONE]"},
		{"tel +44 20 7946 0958", "tel [REDACTED:PHONE]"},
		{"tel 06 12 34 56 78", "tel [REDACTED:PHONE]"},
		{"ssn 123-45-6789", "ssn [REDACTED:NATIONAL_ID]"},
		{"nino QQ 12 34 56 C", "nino [REDACTED:NATIONAL_ID]"},
		{"order O-00001 refund 5000 cents due 2026-11-15", "order O-00001 refund 5000 cents due 2026-11-15"},
		{"customer C-0042, invoice 2026-000123", "customer C-0042, invoice 2026-000123"},
		// Regression (M2 review): trailing digits after a valid card must not hide it.
		{"card 4111 1111 1111 1111 12/27", "card [REDACTED:CARD] 12/27"},
		{"card 4111111111111111 123", "card [REDACTED:CARD] 123"},
		{"tracking 12345678901234567890", "tracking 12345678901234567890"},
	}
	for _, c := range cases {
		if got := r.String(c.in); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactJSONKeepsStructure(t *testing.T) {
	r := redact.New()
	in := json.RawMessage(`{"customer_id":"C-0001","card":"4111 1111 1111 1111","nested":[{"email":"a.b@example.com"}],"amount_cents":5000,"pan":4111111111111111}`)
	got := r.JSON(in)
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("redacted JSON invalid: %v (%s)", err, got)
	}
	if m["card"] != "[REDACTED:CARD]" || m["pan"] != "[REDACTED:CARD]" || m["customer_id"] != "C-0001" || m["amount_cents"] != float64(5000) {
		t.Fatalf("redacted = %s", got)
	}
	if !strings.Contains(string(got), `"email":"[REDACTED:EMAIL]"`) {
		t.Fatalf("nested email not redacted: %s", got)
	}
	if got := string(r.JSON(json.RawMessage(`{"a@example.com":1,"b@example.com":2}`))); !strings.Contains(got, `"[REDACTED:EMAIL]#2"`) {
		t.Fatalf("colliding redacted keys: %s", got)
	}
	if got := string(r.JSON(json.RawMessage(`{"alice@example.com":1}`))); strings.Contains(got, "alice") {
		t.Fatalf("map key not redacted: %s", got)
	}
	if string(r.JSON(json.RawMessage(`not json`))) != `"[REDACTED:UNPARSEABLE]"` {
		t.Fatal("invalid JSON must be replaced, never passed through")
	}
}

func TestRedactorProperties(t *testing.T) {
	r := redact.New()
	t.Run("idempotent", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			s := rapid.StringMatching(`[ a-zA-Z0-9@.+()-]{0,80}`).Draw(t, "s")
			once := r.String(s)
			if twice := r.String(once); twice != once {
				t.Fatalf("not idempotent: %q -> %q -> %q", s, once, twice)
			}
		})
	})
	t.Run("never touches Luhn-invalid card numbers", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			digits := rapid.StringMatching(`4[0-9]{15}`).Draw(t, "digits")
			if redact.Luhn(digits) {
				digits = digits[:15] + string('0'+(digits[15]-'0'+1)%10) // make it fail
			}
			grouped := digits[:4] + " " + digits[4:8] + " " + digits[8:12] + " " + digits[12:]
			for _, s := range []string{digits, grouped} {
				if got := r.String("pay " + s); strings.Contains(got, "CARD") {
					t.Fatalf("Luhn-invalid %q redacted: %q", s, got)
				}
			}
		})
	})
	t.Run("always redacts Luhn-valid card numbers", func(t *testing.T) {
		rapid.Check(t, func(t *rapid.T) {
			body := rapid.StringMatching(`5[1-5][0-9]{13}`).Draw(t, "body")
			card := body + string('0'+redact.LuhnCheckDigit(body))
			if got := r.String("pay " + card + " now"); got != "pay [REDACTED:CARD] now" {
				t.Fatalf("%q -> %q", card, got)
			}
		})
	})
}
