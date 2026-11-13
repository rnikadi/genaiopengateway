// Package redact removes personal data and secrets from request and
// response bodies before they are stored (docs/body-logging.md).
package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Builtin redaction names.
const (
	Email      = "email"
	Phone      = "phone"
	CreditCard = "credit_card"
	IBAN       = "iban"
	USSSN      = "us_ssn"
	Secret     = "secret"
	Custom     = "custom"
)

// Builtins lists every builtin, in the order they are applied. Secrets
// go first, so a key is not half-eaten by the phone pattern.
var Builtins = []string{Secret, Email, CreditCard, IBAN, USSSN, Phone}

type rule struct {
	name  string
	re    *regexp.Regexp
	valid func(string) bool // nil accepts every match
	// trigger, when set, is a byte every match contains, and no match
	// reaches further than reach bytes from one. The regexp then runs
	// only on the text around those bytes, which is much faster on long
	// text with few candidates.
	trigger func(byte) bool
	reach   int
	// literals, when set, are strings one of which every match contains;
	// text with none of them is skipped.
	literals []string
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAt(c byte) bool    { return c == '@' }

// digitReach bounds the distance from any digit of a card number, IBAN,
// SSN or phone number to the ends of the match; the longest, an IBAN
// with spaces, is under 48 bytes.
const digitReach = 64

var builtinRules = map[string]rule{
	Secret: {Secret, regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----` +
		`|\bsk-[A-Za-z0-9_-]{20,}` +
		`|\bAKIA[0-9A-Z]{16}\b` +
		`|\bgh[pousr]_[A-Za-z0-9]{30,}` +
		`|\bgithub_pat_[A-Za-z0-9_]{30,}` +
		`|\bxox[abprs]-[A-Za-z0-9-]{10,}` +
		`|\bAIza[0-9A-Za-z_-]{35}` +
		`|\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), nil, nil, 0,
		[]string{"-----BEGIN ", "sk-", "AKIA", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "xoxa-", "xoxb-", "xoxp-", "xoxr-", "xoxs-", "AIza", "eyJ"}},
	// Emails hold no whitespace, so the text around each @ up to the next
	// whitespace holds the whole match.
	Email:      {Email, regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`), nil, isAt, 1, nil},
	CreditCard: {CreditCard, regexp.MustCompile(`\b\d(?:[ -]?\d){12,18}\b`), luhn, isDigit, digitReach, nil},
	IBAN:       {IBAN, regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`), ibanValid, isDigit, digitReach, nil},
	USSSN:      {USSSN, regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), ssnValid, isDigit, digitReach, nil},
	// International numbers with a leading +, and North American ones
	// written with separators. Dates and times do not match.
	Phone: {Phone, regexp.MustCompile(`\+\d{1,3}(?:[ .-]?\(?\d{1,4}\)?){2,5}\d\b|(?:\(\d{3}\) ?|\b\d{3}[.-])\d{3}[.-]\d{4}\b`), phoneValid, isDigit, digitReach, nil},
}

// Config selects the redactions to apply.
type Config struct {
	// Builtin names the builtins to apply; nil means all of them.
	Builtin []string
	// Patterns are extra RE2 expressions, replaced by [REDACTED:custom].
	Patterns []string
}

// Redactor applies one Config.
type Redactor struct {
	rules []rule
}

// New compiles a Config.
func New(c Config) (*Redactor, error) {
	names := c.Builtin
	if names == nil {
		names = Builtins
	}
	r := &Redactor{}
	for _, name := range Builtins {
		if slices.Contains(names, name) {
			r.rules = append(r.rules, builtinRules[name])
		}
	}
	for _, n := range names {
		if _, ok := builtinRules[n]; !ok {
			return nil, fmt.Errorf("unknown redaction %q", n)
		}
	}
	for _, p := range c.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redaction pattern %q: %w", p, err)
		}
		r.rules = append(r.rules, rule{name: Custom, re: re})
	}
	return r, nil
}

// Counts records how many values of each kind were redacted.
type Counts map[string]int

// String redacts free text.
func (r *Redactor) String(s string, counts Counts) string {
	for _, rl := range r.rules {
		s = rl.apply(s, counts)
	}
	return s
}

func (rl *rule) apply(s string, counts Counts) string {
	if rl.literals != nil && !containsAny(s, rl.literals) {
		return s
	}
	replace := func(m string) string {
		n := len(m)
		if rl.valid != nil {
			n = validPrefix(m, rl.valid)
		}
		if n == 0 {
			return m
		}
		counts[rl.name]++
		return "[REDACTED:" + rl.name + "]" + m[n:]
	}
	if rl.trigger == nil {
		return rl.re.ReplaceAllStringFunc(s, replace)
	}
	var b strings.Builder
	done := 0
	for _, w := range windows(s, rl.trigger, rl.reach) {
		if done == 0 {
			b.Grow(len(s))
		}
		b.WriteString(s[done:w[0]])
		b.WriteString(rl.re.ReplaceAllStringFunc(s[w[0]:w[1]], replace))
		done = w[1]
	}
	if done == 0 {
		return s
	}
	b.WriteString(s[done:])
	return b.String()
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// windows returns the spans of s within reach of a trigger byte, widened
// to whitespace so that word boundaries at their edges are the same as in
// s, and merged where they touch.
func windows(s string, trigger func(byte) bool, reach int) [][2]int {
	var out [][2]int
	for i := 0; i < len(s); i++ {
		if !trigger(s[i]) {
			continue
		}
		end := min(i+reach+1, len(s))
		n := len(out)
		if n > 0 && out[n-1][1] >= len(s) {
			break // the last window runs to the end
		}
		if n > 0 && i-reach <= out[n-1][1] {
			end = max(end, out[n-1][1])
		} else {
			start, floor := max(i-reach, 0), 0
			if n > 0 {
				floor = out[n-1][1]
			}
			for start > floor && !isSpace(s[start-1]) {
				start--
			}
			out = append(out, [2]int{start, end})
			n++
		}
		for end < len(s) && !isSpace(s[end]) {
			end++
		}
		out[n-1][1] = end
	}
	return out
}

func isSpace(c byte) bool { return c == ' ' || c == '\n' || c == '\t' || c == '\r' }

// validPrefix returns the length of the longest prefix of m that is valid,
// trying m and then m cut at each separator, or 0. A greedy match can run
// into a following number or word ("4111 1111 1111 1111 12").
func validPrefix(m string, valid func(string) bool) int {
	if valid(m) {
		return len(m)
	}
	for i := len(m) - 1; i > 0; i-- {
		if (m[i] == ' ' || m[i] == '-') && valid(m[:i]) {
			return i
		}
	}
	return 0
}

// Body redacts a JSON body: every string value is redacted as text, the
// values of fields named like secrets are replaced whole, and inline
// base64 media is replaced by a placeholder. A body that is not JSON is
// redacted as text. The result is truncated to max bytes.
func (r *Redactor) Body(raw []byte, max int) (out []byte, counts Counts, truncated bool) {
	counts = Counts{}
	// Bound the work: anything beyond 4×max would be cut anyway.
	if limit := 4 * max; len(raw) > limit {
		raw, truncated = raw[:limit], true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err == nil && !dec.More() {
		v = r.walk(v, "", counts)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(v)
		out = bytes.TrimRight(buf.Bytes(), "\n")
	} else {
		out = []byte(r.String(string(raw), counts))
	}
	if len(out) > max {
		out, truncated = cut(out, max), true
	}
	return out, counts, truncated
}

// cut truncates at max bytes without splitting a UTF-8 character.
func cut(b []byte, max int) []byte {
	for max > 0 && max < len(b) && b[max]&0xC0 == 0x80 {
		max--
	}
	return b[:max]
}

func (r *Redactor) walk(v any, field string, counts Counts) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok && secretField(k) && s != "" {
				counts[Secret]++
				t[k] = "[REDACTED:secret]"
				continue
			}
			t[k] = r.walk(x, k, counts)
		}
		return t
	case []any:
		for i, x := range t {
			t[i] = r.walk(x, field, counts)
		}
		return t
	case string:
		if p, ok := media(t); ok {
			return p
		}
		return r.String(t, counts)
	}
	return v
}

// secretField reports whether a field name suggests its value is a secret.
// Token counts such as max_tokens are not.
func secretField(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case "api_key", "apikey", "password", "passwd", "secret", "client_secret", "authorization",
		"token", "access_token", "refresh_token", "id_token", "private_key":
		return true
	}
	return strings.HasSuffix(n, "_api_key") || strings.HasSuffix(n, "_secret") ||
		strings.HasSuffix(n, "_password") || strings.HasSuffix(n, "_token")
}

// media replaces inline base64 media (data URLs, or long base64 strings
// such as audio and image payloads) with a placeholder.
func media(s string) (string, bool) {
	if strings.HasPrefix(s, "data:") {
		if head, data, ok := strings.Cut(s, ";base64,"); ok {
			return fmt.Sprintf("[omitted: %s, %d bytes]", strings.TrimPrefix(head, "data:"), len(data)*3/4), true
		}
	}
	if len(s) >= 1024 && isBase64(s) {
		return fmt.Sprintf("[omitted: base64, %d bytes]", len(s)*3/4), true
	}
	return "", false
}

func isBase64(s string) bool {
	for _, c := range s {
		if !(c < unicode.MaxASCII && (c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' || c == '-' || c == '_')) {
			return false
		}
	}
	return true
}

func digits(s string) []int {
	var d []int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			d = append(d, int(c-'0'))
		}
	}
	return d
}

func luhn(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	sum := 0
	for i := range d {
		x := d[len(d)-1-i]
		if i%2 == 1 {
			if x *= 2; x > 9 {
				x -= 9
			}
		}
		sum += x
	}
	return sum%10 == 0
}

func ibanValid(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	rearranged := s[4:] + s[:4]
	rem := 0
	for _, c := range rearranged {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A'+10)) % 97
		default:
			return false
		}
	}
	return rem == 1
}

func ssnValid(s string) bool {
	if len(s) != 11 {
		return false
	}
	area, group, serial := s[0:3], s[4:6], s[7:11]
	return area != "000" && area != "666" && area[0] != '9' && group != "00" && serial != "0000"
}

// phoneValid wants 9 to 15 digits, which skips dates, times and versions.
func phoneValid(s string) bool {
	n := len(digits(s))
	return n >= 9 && n <= 15
}
