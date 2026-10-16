package canonicaljson_test

import (
	"encoding/json"
	"testing"

	"pgregory.net/rapid"

	"github.com/ROBERTG99/mcp-gateway/pkg/canonicaljson"
)

func TestCanonicalizeRFC8785Vectors(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{`{ "z" : [ 3 , {"y":true,"x":null} ] }`, `{"z":[3,{"x":null,"y":true}]}`},
		{`1.0`, `1`},
		{`-0`, `0`},
		{`1e21`, `1e+21`},
		{`1e20`, `100000000000000000000`},
		{`0.000001`, `0.000001`},
		{`1e-7`, `1e-7`},
		{`123.456e2`, `12345.6`},
		{`"\u00e9\u2028<>&\u001f"`, "\"\u00e9\u2028<>&\\u001f\""},
		{`"a\"b\\c\n\t\b\f\r"`, `"a\"b\\c\n\t\b\f\r"`},
		// RFC 8785 sorts keys by UTF-16 code units: 0x000D < 0x20AC (euro) < 0xD83D (emoji high surrogate).
		{`{"\ud83d\ude00":1,"\u20ac":2,"\r":3}`, "{\"\\r\":3,\"\u20ac\":2,\"\U0001F600\":1}"},
		{`[1, 2.5, -3e-2, 333333333.33333329]`, `[1,2.5,-0.03,333333333.3333333]`},
		{`9007199254740992`, `9007199254740992`},
		{`"\ufffd"`, "\"\ufffd\""},
	}
	for _, c := range cases {
		got, err := canonicaljson.Canonicalize([]byte(c.in))
		if err != nil {
			t.Fatalf("Canonicalize(%s): %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("Canonicalize(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestCanonicalizeRejectsInvalid(t *testing.T) {
	for _, in := range []string{``, `{`, `{"a":1}{`, `[1,]`, `{"a":1,"a":2}`, `NaN`,
		`"\ud800"`, `{"\udc00":1}`, "\"\xff\"", `9007199254740993`, `-9007199254740993`} {
		if _, err := canonicaljson.Canonicalize([]byte(in)); err == nil {
			t.Errorf("Canonicalize(%q) accepted invalid input", in)
		}
	}
}

// Property: the canonical hash does not depend on key order or whitespace.
func TestCanonicalHashIndependentOfKeyOrder(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := rapid.MapOf(rapid.StringMatching(`[a-zA-Z0-9_é€]{0,8}`),
			rapid.OneOf(rapid.Just[any](nil), rapid.Bool().AsAny(), rapid.Int64Range(-1<<52, 1<<52).AsAny(),
				rapid.StringN(0, 12, -1).AsAny(), rapid.Float64Range(-1e6, 1e6).AsAny())).Draw(t, "m")
		a, _ := json.Marshal(m)
		// Re-encode with reversed insertion order and indentation.
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		var b []byte
		b = append(b, "{\n"...)
		for i := len(keys) - 1; i >= 0; i-- {
			kb, _ := json.Marshal(keys[i])
			vb, _ := json.Marshal(m[keys[i]])
			b = append(b, "  "...)
			b = append(b, kb...)
			b = append(b, " : "...)
			b = append(b, vb...)
			if i > 0 {
				b = append(b, ",\n"...)
			}
		}
		b = append(b, "\n}"...)
		ha, err := canonicaljson.SHA256Hex(a)
		if err != nil {
			t.Fatal(err)
		}
		hb, err := canonicaljson.SHA256Hex(b)
		if err != nil {
			t.Fatal(err)
		}
		if ha != hb {
			t.Fatalf("hash differs for %s vs %s", a, b)
		}
	})
}
