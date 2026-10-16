// Package redact removes personal data from strings, JSON and log attributes before anything
// is written to the audit log, webhooks or logs. Detectors: card numbers (Luhn-checked), IBANs
// (mod-97-checked), national-ID-like numbers (US SSN, UK NINO), email addresses and phone numbers.
// Matches become "[REDACTED:<TYPE>]". Redaction is idempotent: markers never match a detector.
package redact

import (
	"bytes"
	"encoding/json"
	"math/big"
	"regexp"
	"strings"
)

// Types of personal data.
const (
	Card       = "CARD"
	IBAN       = "IBAN"
	NationalID = "NATIONAL_ID"
	Email      = "EMAIL"
	Phone      = "PHONE"
	Token      = "TOKEN"
	Secret     = "SECRET"
)

// Marker returns the replacement text for a type.
func Marker(typ string) string { return "[REDACTED:" + typ + "]" }

type detector struct {
	typ   string
	re    *regexp.Regexp
	valid func(match string) bool
}

// Redactor applies the detectors in a fixed order (most specific first).
type Redactor struct {
	detectors []detector
}

// New returns a redactor with the built-in detectors.
func New() *Redactor {
	return &Redactor{detectors: []detector{
		{typ: Token, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)},
		{typ: Token, re: regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{16,}=*`)},
		{typ: IBAN, re: regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]){11,30}\b`), valid: validIBAN},
		{typ: Card, re: regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`)},
		{typ: NationalID, re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), valid: validSSN},
		{typ: NationalID, re: regexp.MustCompile(`\b[A-Z]{2} ?\d{2} ?\d{2} ?\d{2} ?[A-D]\b`)},
		{typ: Email, re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)},
		{typ: Phone, re: regexp.MustCompile(`\+\d{1,3}(?:[ .-]?\(?\d{1,4}\)?){2,5}`), valid: phoneDigits},
		{typ: Phone, re: regexp.MustCompile(`\(\d{3}\)[ .-]?\d{3}[ .-]\d{4}\b`)},
		{typ: Phone, re: regexp.MustCompile(`\b\d{3}[.-]\d{3}[.-]\d{4}\b`)},
		{typ: Phone, re: regexp.MustCompile(`\b0\d(?:[ .-]\d{2}){4}\b`)},
	}}
}

// Match is one detected span.
type Match struct {
	Type  string
	Value string
}

// String redacts s.
func (r *Redactor) String(s string) string {
	out, _ := r.redact(s)
	return out
}

// Detect returns the detected spans in s (in detector order) without changing it.
func (r *Redactor) Detect(s string) []Match {
	_, m := r.redact(s)
	return m
}

func (r *Redactor) redact(s string) (string, []Match) {
	var found []Match
	for _, d := range r.detectors {
		s = d.re.ReplaceAllStringFunc(s, func(m string) string {
			if d.typ == IBAN {
				if ok, n := ibanPrefix(m); ok {
					found = append(found, Match{Type: IBAN, Value: m[:n]})
					return Marker(IBAN) + m[n:]
				}
				return m
			}
			if d.typ == Card {
				start, end, ok := cardSpan(m)
				if !ok {
					return m
				}
				found = append(found, Match{Type: Card, Value: m[start:end]})
				return m[:start] + Marker(Card) + m[end:]
			}
			if d.valid != nil && !d.valid(m) {
				return m
			}
			found = append(found, Match{Type: d.typ, Value: m})
			return Marker(d.typ)
		})
	}
	return s, found
}

// JSON redacts every string (and card-like number) in a JSON document, keeping its structure.
// Unparseable input is replaced entirely: it is never passed through.
func (r *Redactor) JSON(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return json.RawMessage(`"` + Marker("UNPARSEABLE") + `"`)
	}
	out, err := json.Marshal(r.Value(v))
	if err != nil {
		return json.RawMessage(`"` + Marker("UNPARSEABLE") + `"`)
	}
	return out
}

// Value redacts a decoded JSON value (maps, slices, strings, numbers).
func (r *Redactor) Value(v any) any {
	switch x := v.(type) {
	case string:
		return r.String(x)
	case json.Number:
		if red := r.String(x.String()); red != x.String() {
			return red
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[r.String(k)] = r.Value(val) // keys can hold personal data too
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = r.Value(val)
		}
		return out
	default:
		return v
	}
}

func digitsOf(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// Luhn reports whether a digit string passes the Luhn checksum.
func Luhn(digits string) bool {
	if len(digits) < 2 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// LuhnCheckDigit returns the digit that makes body+digit Luhn-valid.
func LuhnCheckDigit(body string) byte {
	for d := byte(0); d <= 9; d++ {
		if Luhn(body + string('0'+d)) {
			return d
		}
	}
	return 0
}

// cardSpan finds a Luhn-valid card number of 13-19 digits inside a digit run: the whole run, or
// a sequence of whole separator-delimited groups (so "4111 1111 1111 1111 12" still redacts the
// card). An unseparated run is only a card as a whole, which keeps long reference numbers intact.
func cardSpan(m string) (start, end int, ok bool) {
	if d := digitsOf(m); len(d) <= 19 && Luhn(d) {
		return 0, len(m), true
	}
	type group struct{ start, end int }
	var groups []group
	gs := 0
	for i := 0; i <= len(m); i++ {
		if i == len(m) || m[i] == ' ' || m[i] == '-' {
			if i > gs {
				groups = append(groups, group{gs, i})
			}
			gs = i + 1
		}
	}
	for i := range groups {
		for j := len(groups) - 1; j >= i; j-- {
			span := m[groups[i].start:groups[j].end]
			if d := digitsOf(span); len(d) >= 13 && len(d) <= 19 && Luhn(d) {
				return groups[i].start, groups[j].end, true
			}
		}
	}
	return 0, 0, false
}

// ibanPrefix finds the longest prefix of m (at least 15 characters without spaces) that is a valid
// IBAN, so a trailing uppercase word is not swallowed. It returns the prefix length in m.
func ibanPrefix(m string) (bool, int) {
	for end := len(m); end > 0; end-- {
		if m[end-1] == ' ' {
			continue
		}
		cand := strings.ReplaceAll(m[:end], " ", "")
		if len(cand) < 15 {
			break
		}
		if validIBAN(cand) {
			return true, end
		}
	}
	return false, 0
}

func validIBAN(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	var b strings.Builder
	for _, c := range rearranged {
		switch {
		case c >= '0' && c <= '9':
			b.WriteRune(c)
		case c >= 'A' && c <= 'Z':
			b.WriteString(big.NewInt(int64(c-'A') + 10).String())
		default:
			return false
		}
	}
	n, ok := new(big.Int).SetString(b.String(), 10)
	return ok && new(big.Int).Mod(n, big.NewInt(97)).Int64() == 1
}

func validSSN(m string) bool {
	area, group, serial := m[0:3], m[4:6], m[7:11]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

func phoneDigits(m string) bool {
	n := len(digitsOf(m))
	return n >= 9 && n <= 15
}
