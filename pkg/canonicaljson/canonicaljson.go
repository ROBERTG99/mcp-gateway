// Package canonicaljson implements the JSON Canonicalization Scheme (RFC 8785): object keys sorted
// by UTF-16 code units, no insignificant whitespace, ECMAScript number formatting and minimal
// string escaping. The gateway hashes tool arguments with it (args_sha256) and chains audit
// events with it, so equal values always hash equally regardless of key order or formatting.
package canonicaljson

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxSafeInteger is the largest integer an IEEE-754 double represents exactly (2^53).
const MaxSafeInteger = 1 << 53

// Canonicalize parses raw JSON and returns its RFC 8785 canonical form. Input must be I-JSON
// (RFC 7493): duplicate object keys, trailing data, invalid UTF-8, unpaired surrogate escapes and
// integers beyond ±2^53 are rejected, so two different inputs never canonicalise to the same bytes.
func Canonicalize(raw []byte) ([]byte, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("canonicaljson: input is not valid UTF-8")
	}
	// encoding/json maps unpaired surrogate escapes to U+FFFD; only accept U+FFFD when the input
	// spelled it (literally or as \ufffd).
	allowReplacement := bytes.ContainsRune(raw, utf8.RuneError) || bytes.Contains(bytes.ToLower(raw), []byte(`\ufffd`))
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var buf bytes.Buffer
	if err := writeValue(dec, &buf, allowReplacement); err != nil {
		return nil, fmt.Errorf("canonicaljson: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("canonicaljson: trailing data after JSON value")
	}
	return buf.Bytes(), nil
}

// Marshal encodes v with encoding/json and canonicalizes the result.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonicaljson: marshal: %w", err)
	}
	return Canonicalize(raw)
}

// SHA256Hex returns the lowercase hex SHA-256 of the canonical form of raw.
func SHA256Hex(raw []byte) (string, error) {
	c, err := Canonicalize(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return hex.EncodeToString(sum[:]), nil
}

func writeValue(dec *json.Decoder, buf *bytes.Buffer, allowFFFD bool) error {
	tok, err := dec.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("unexpected end of JSON input")
		}
		return err
	}
	return writeToken(dec, buf, tok, allowFFFD)
}

func writeToken(dec *json.Decoder, buf *bytes.Buffer, tok json.Token, allowFFFD bool) error {
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			return writeObject(dec, buf, allowFFFD)
		case '[':
			return writeArray(dec, buf, allowFFFD)
		default:
			return fmt.Errorf("unexpected delimiter %q", v)
		}
	case string:
		if !allowFFFD && strings.ContainsRune(v, utf8.RuneError) {
			return errors.New("string contains an unpaired surrogate escape")
		}
		writeString(buf, v)
	case json.Number:
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return fmt.Errorf("number %s is not representable as an IEEE-754 double", v)
		}
		if !strings.ContainsAny(string(v), ".eE") {
			n, err := strconv.ParseInt(string(v), 10, 64)
			if err != nil || n > MaxSafeInteger || n < -MaxSafeInteger {
				return fmt.Errorf("integer %s is outside the exactly representable range ±2^53", v)
			}
		}
		buf.WriteString(FormatNumber(f))
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case nil:
		buf.WriteString("null")
	default:
		return fmt.Errorf("unexpected token %T", tok)
	}
	return nil
}

type member struct {
	key   string
	units []uint16
	value []byte
}

func writeObject(dec *json.Decoder, buf *bytes.Buffer, allowFFFD bool) error {
	var members []member
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("object key is %T, want string", tok)
		}
		if !allowFFFD && strings.ContainsRune(key, utf8.RuneError) {
			return errors.New("object key contains an unpaired surrogate escape")
		}
		if seen[key] {
			return fmt.Errorf("duplicate object key %q", key)
		}
		seen[key] = true
		var vb bytes.Buffer
		if err := writeValue(dec, &vb, allowFFFD); err != nil {
			return err
		}
		members = append(members, member{key: key, units: utf16.Encode([]rune(key)), value: vb.Bytes()})
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return err
	}
	slices.SortFunc(members, func(a, b member) int { return slices.Compare(a.units, b.units) })
	buf.WriteByte('{')
	for i, m := range members {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeString(buf, m.key)
		buf.WriteByte(':')
		buf.Write(m.value)
	}
	buf.WriteByte('}')
	return nil
}

func writeArray(dec *json.Decoder, buf *bytes.Buffer, allowFFFD bool) error {
	buf.WriteByte('[')
	first := true
	for dec.More() {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		if err := writeValue(dec, buf, allowFFFD); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // closing ']'
		return err
	}
	buf.WriteByte(']')
	return nil
}

const hexDigits = "0123456789abcdef"

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[r>>4])
				buf.WriteByte(hexDigits[r&0xf])
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// FormatNumber renders a finite double like ECMAScript's Number.prototype.toString.
func FormatNumber(f float64) string {
	if f == 0 {
		return "0" // also for -0
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±XX, shortest round-trip digits
	mant, expStr, _ := strings.Cut(sci, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k, n := len(digits), exp+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	e := n - 1
	es := "+"
	if e < 0 {
		es, e = "-", -e
	}
	if k == 1 {
		return sign + digits + "e" + es + strconv.Itoa(e)
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + es + strconv.Itoa(e)
}
