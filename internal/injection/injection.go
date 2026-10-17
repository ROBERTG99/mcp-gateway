// Package injection scores upstream output for prompt-injection signals. It is a deterministic
// heuristic (defence in depth, not a solution): text is normalised (NFKC, a confusables map,
// hidden characters removed, spaced letters and leetspeak folded) and each signal adds a fixed
// weight; the score is clipped to 1.0.
package injection

import (
	"encoding/base64"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Signal weights (spec §4.5).
const (
	SignalOverride   = "override_phrase"
	SignalHidden     = "hidden_unicode"
	SignalTool       = "tool_reference"
	SignalRole       = "role_marker"
	SignalImperative = "imperative_density"
	SignalLink       = "external_link"
	SignalBase64     = "base64_blob"
)

// Weights returns the weight of every signal.
func Weights() map[string]float64 {
	return map[string]float64{SignalOverride: 0.35, SignalHidden: 0.3, SignalTool: 0.2, SignalRole: 0.2,
		SignalImperative: 0.15, SignalLink: 0.15, SignalBase64: 0.1}
}

// Result is a score in [0, 1] and the signals that fired (sorted).
type Result struct {
	Score   float64  `json:"score"`
	Signals []string `json:"signals"`
}

// Options configure the scorer.
type Options struct {
	// ToolNames are namespaced tool names (server.tool) whose mention is a tool_reference signal.
	// When empty, any "<server>.<verb>_<object>" token counts.
	ToolNames []string
	// ImperativesPer100 is the imperative-verb density threshold (default 6 per 100 tokens).
	ImperativesPer100 float64
}

// Scorer computes injection scores. Safe for concurrent use.
type Scorer struct {
	opts      Options
	weights   map[string]float64
	toolNames map[string]bool
}

// New builds a scorer.
func New(opts Options) *Scorer {
	if opts.ImperativesPer100 == 0 {
		opts.ImperativesPer100 = 6
	}
	s := &Scorer{opts: opts, weights: Weights(), toolNames: map[string]bool{}}
	for _, n := range opts.ToolNames {
		s.toolNames[strings.ToLower(n)] = true
	}
	return s
}

var (
	overridePhrases = []*regexp.Regexp{
		regexp.MustCompile(`\b(ignore|disregard|forget|skip|bypass|override)\s+(all\s+|any\s+|the\s+|your\s+|my\s+|of\s+)*(previous|prior|above|earlier|preceding|existing|original|system|current)\s+(instructions?|rules?|guidance|guidelines|messages?|prompts?|directions?|context|policies|policy)\b`),
		regexp.MustCompile(`\b(ignore|disregard|forget)\s+(all|everything)\s+(above|before|previous(ly)?|prior)\b`),
		regexp.MustCompile(`\b(ignore|disregard)\s+the\s+above\b`),
		regexp.MustCompile(`\b(forget|disregard)\s+(your|all)\s+(previous\s+)?(guidance|instructions|rules|training)\b`),
		regexp.MustCompile(`\bnew\s+(system\s+)?instructions?\s*:`),
		regexp.MustCompile(`\byou\s+are\s+(now\s+)?(in\s+)?(an?\s+)?(admin|developer|debug|god|unrestricted|jailbreak|dan)\b`),
		// Credential solicitation, instructions hidden in an encoding, and bypassing a human check.
		regexp.MustCompile(`\b(submit|send|share|paste|post|forward|reveal)\s+(the|your|any|all)\s+((session|access|bearer|api)\s+tokens?|passwords?|credentials?|(api|secret|private|ssh)\s+keys?|secrets?|tokens?)\b`),
		regexp.MustCompile(`\b(decode|decrypt|deobfuscate|unscramble)\b[^.]{0,60}\b(follow|execute|obey|run)\b`),
		regexp.MustCompile(`\b(skip|bypass|disable|avoid)\s+(the\s+)?(approvals?|human\s+(review|approval))\b`),
		regexp.MustCompile(`\b(reveal|print|show|output)\s+(your|the)\s+(system\s+prompt|hidden\s+instructions|instructions)\b`),
		regexp.MustCompile(`\bdo\s+not\s+(tell|inform|alert|notify)\s+the\s+(user|human|operator|customer)\b`),
		regexp.MustCompile(`\b(the\s+)?real\s+(task|instructions?)\s+(is|are)\b`),
		regexp.MustCompile(`\bimportant\s+system\s+(notice|message|override)\b`),
	}
	roleMarkers = []*regexp.Regexp{
		regexp.MustCompile(`(?im)^\s*(system|assistant|developer|user)\s*:`),
		regexp.MustCompile(`(?i)<\|\s*(system|assistant|user|im_start|im_end|endoftext)\s*\|>`),
		regexp.MustCompile(`(?i)\[/?(inst|sys)\]|<<\s*sys\s*>>`),
		regexp.MustCompile(`(?im)^\s*#{2,}\s*(system|instruction|instructions)\b`),
		// A speaker label after a sentence ("... on Monday. SYSTEM: ..."), a "system instructions:"
		// header, or text addressed to the model ("To the AI:", "for the assistant:").
		regexp.MustCompile(`(?i)[.!?]\s+(system|assistant|developer)\s*:`),
		regexp.MustCompile(`(?i)\b(system|developer)\s+(prompt|instructions?|message)\s*:`),
		regexp.MustCompile(`(?i)\b(to|for)\s+(the|any|all)\s+(ai|assistant|llm|language\s+model|ai\s+agents?)(\s+reading\s+this)?\s*:`),
	}
	genericTool = regexp.MustCompile(`\b[a-z][a-z0-9_]{1,31}\.(get|add|record|issue|refund|create|delete|update|send|list|call|transfer|approve)_[a-z_]+\b`)
	urlRe       = regexp.MustCompile(`(?i)\bhttps?://\S+`)
	linkQuery   = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+\?[^\s"'<>]*=`)
	base64Blob  = regexp.MustCompile(`[A-Za-z0-9+/]{40,}={0,2}`)
	tokenRe     = regexp.MustCompile(`[\p{L}\p{N}_.]+`)
	spacedRe    = regexp.MustCompile(`\b(?:[a-z][ ._\-]){2,}[a-z]\b`)
	sentenceRe  = regexp.MustCompile(`[.!?\n]+`)
	imperatives = map[string]bool{
		"ignore": true, "disregard": true, "forget": true, "override": true, "bypass": true, "call": true, "run": true,
		"execute": true, "send": true, "forward": true, "delete": true, "remove": true, "approve": true, "refund": true,
		"issue": true, "transfer": true, "export": true, "upload": true, "reveal": true, "print": true, "grant": true,
		"email": true, "pay": true, "wire": true, "invoke": true, "submit": true, "disable": true, "tell": true,
		"approve_all": true, "do": true, "write": true, "set": true, "change": true, "update": true,
	}
	clauseSplit = regexp.MustCompile(`[.!?;:\n,]+|\b(and|then)\b`)
	fillers     = map[string]bool{"please": true, "now": true, "immediately": true, "also": true, "just": true}
	leet        = strings.NewReplacer("0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t", "@", "a", "$", "s")
)

// confusables maps common Cyrillic and Greek look-alikes to Latin letters.
var confusables = map[rune]rune{
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y', 'х': 'x', 'і': 'i', 'ј': 'j', 'ѕ': 's', 'ԁ': 'd',
	'һ': 'h', 'ԛ': 'q', 'ԝ': 'w', 'ɡ': 'g', 'ո': 'n', 'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H',
	'О': 'O', 'Р': 'P', 'С': 'C', 'Т': 'T', 'Х': 'X', 'І': 'I', 'α': 'a', 'ο': 'o', 'ι': 'i', 'ν': 'v', 'ε': 'e',
	'κ': 'k', 'ρ': 'p', 'τ': 't', 'υ': 'u', 'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ι': 'I', 'Κ': 'K', 'Μ': 'M', 'Ν': 'N',
	'Ο': 'O', 'Ρ': 'P', 'Τ': 'T', 'Χ': 'X',
}

// isHidden reports zero-width, bidi-control, tag and other invisible format characters.
func isHidden(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E, r >= 0x2060 && r <= 0x2064,
		r >= 0x2066 && r <= 0x2069, r == 0xFEFF, r == 0x00AD, r == 0x180E, r >= 0xE0000 && r <= 0xE007F:
		return true
	}
	return unicode.Is(unicode.Cf, r)
}

// normalize returns the text folded for matching and whether hidden characters were present.
func normalize(text string) (string, bool) {
	hidden := false
	var b strings.Builder
	for _, r := range norm.NFKC.String(text) {
		if isHidden(r) {
			hidden = true
			continue
		}
		if c, ok := confusables[r]; ok {
			r = c
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String()), hidden
}

// Score evaluates text.
func (s *Scorer) Score(text string) Result { return s.score(text, 0) }

// dense reports whether text of at least minTokens tokens has more clause-initial imperatives per
// 100 tokens than the threshold.
func (s *Scorer) dense(text string, minTokens int) bool {
	tokens := tokenRe.FindAllString(text, -1)
	return len(tokens) >= minTokens && float64(countImperatives(text))*100/float64(len(tokens)) > s.opts.ImperativesPer100
}

func (s *Scorer) score(text string, depth int) Result {
	norm, hidden := normalize(text)
	// Variants for phrase matching: spaced-out letters joined, and leetspeak folded.
	despaced := spacedRe.ReplaceAllStringFunc(norm, func(m string) string {
		return strings.Map(func(r rune) rune {
			if r == ' ' || r == '.' || r == '_' || r == '-' {
				return -1
			}
			return r
		}, m)
	})
	variants := []string{norm, despaced, leet.Replace(despaced)}

	fired := map[string]bool{}
	if hidden {
		fired[SignalHidden] = true
	}
	for _, v := range variants {
		for _, re := range overridePhrases {
			if re.MatchString(v) {
				fired[SignalOverride] = true
			}
		}
	}
	for _, v := range variants {
		for _, re := range roleMarkers {
			if re.MatchString(v) {
				fired[SignalRole] = true
			}
		}
	}
	if s.mentionsTool(norm) {
		fired[SignalTool] = true
	}
	if linkQuery.MatchString(text) || linkQuery.MatchString(norm) {
		fired[SignalLink] = true
	}
	for _, m := range base64Blob.FindAllString(text, -1) {
		if decoded, err := base64.StdEncoding.DecodeString(padded(m)); err == nil {
			fired[SignalBase64] = true
			// The payload counts as if it were written in clear (one level deep).
			if depth == 0 && utf8.Valid(decoded) {
				for _, sig := range s.score(string(decoded), 1).Signals {
					fired[sig] = true
				}
			}
			break
		}
	}
	prose := urlRe.ReplaceAllString(norm, " ") // URLs are not prose
	if s.dense(prose, 5) {
		fired[SignalImperative] = true
	}
	// An injected instruction is often one sentence inside ordinary text: a sentence of at least
	// eight tokens is judged on its own density too.
	for _, sentence := range sentenceRe.Split(prose, -1) {
		if s.dense(sentence, 8) {
			fired[SignalImperative] = true
		}
	}
	res := Result{Signals: []string{}}
	for sig := range fired {
		res.Signals = append(res.Signals, sig)
		res.Score += s.weights[sig]
	}
	slices.Sort(res.Signals)
	res.Score = math.Min(1, math.Round(res.Score*100)/100)
	return res
}

// countImperatives counts imperative verbs at the start of a clause: after sentence punctuation
// or a line break, or after "and", "then", "please", "now" (so nouns like "email" mid-sentence
// do not count).
func countImperatives(prose string) int {
	n := 0
	for _, clause := range clauseSplit.Split(prose, -1) {
		words := tokenRe.FindAllString(clause, 3)
		for len(words) > 0 && fillers[words[0]] {
			words = words[1:]
		}
		if len(words) > 0 && imperatives[strings.Trim(words[0], "._")] {
			n++
		}
	}
	return n
}

func (s *Scorer) mentionsTool(norm string) bool {
	if len(s.toolNames) > 0 {
		for name := range s.toolNames {
			if strings.Contains(norm, name) {
				return true
			}
		}
	}
	return genericTool.MatchString(norm)
}

func padded(s string) string {
	if r := len(s) % 4; r != 0 {
		return s + strings.Repeat("=", 4-r)
	}
	return s
}
