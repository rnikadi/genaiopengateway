package guardrail

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// The injection guardrail (docs/prompt-injection.md): in-process checks
// for the wording and markers that prompt injection attacks reuse. It
// catches the known and the lazy, in well under a millisecond, and is
// meant to run before a classifier model, not instead of one.

type injectionRule struct {
	name string
	re   *regexp.Regexp
	// literals, when set, are lower-case strings one of which every
	// match contains; text with none of them skips the expression.
	literals []string
	// scan, when set, counts matches in place of the expression.
	scan func(string) int
}

// hiddenText counts Unicode tag characters, bidirectional overrides and
// runs of four or more zero-width characters.
func hiddenText(s string) int {
	n, run := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] < 0x80 {
			run = 0
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size - 1
		switch {
		case r >= 0xE0000 && r <= 0xE007F, r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069:
			n++
			run = 0
		case r >= 0x200B && r <= 0x200D, r == 0x2060, r == 0xFEFF:
			if run++; run == 4 {
				n++
			}
		default:
			run = 0
		}
	}
	return min(n, 20)
}

const (
	injTarget = `(?:instructions?|directions?|prompts?|rules?|guidelines?|messages?|context|constraints?|programming|training)`
	injBefore = `(?:all|any|every|each|the|your|my|these|those|of|previous|prior|above|earlier|preceding|former|original|initial|system|safety|foregoing)`
)

var injectionRules = map[string]injectionRule{
	// "Ignore all previous instructions" and its many rewordings.
	config.InjectionOverride: {
		re:       regexp.MustCompile(`(?i)\b(?:ignor(?:e|ing)|disregard(?:ing)?|forget(?:ting)?|overrid(?:e|ing)|bypass(?:ing)?|do\s+not\s+follow|don'?t\s+follow|stop\s+following|pay\s+no\s+attention\s+to)\s+(?:` + injBefore + `\s+){1,5}` + injTarget + `\b`),
		literals: []string{"ignor", "disregard", "forget", "overrid", "bypass", "not follow", "t follow", "stop follow", "no attention"},
	},
	// Persona and mode switches that claim the rules no longer apply.
	config.InjectionRole: {
		re: regexp.MustCompile(`(?i)\byou\s+are\s+(?:now\s+)?(?:dan|stan|dude|aim|an?\s+(?:unrestricted|unfiltered|uncensored|jailbroken)\b)` +
			`|\b(?:developer|debug|god|sudo|admin|dan|jailbreak)\s+mode\s+(?:is\s+)?(?:enabled|activated|on|unlocked)\b` +
			`|\b(?:enable|enter|activate|switch\s+to)\s+(?:developer|debug|god|sudo|dan|jailbreak)\s+mode\b` +
			`|\bdo\s+anything\s+now\b` +
			`|\b(?:act|behave|respond|answer)\s+as\s+(?:if\s+you\s+(?:have|had|were)\s+)?(?:an?\s+)?(?:ai\s+|assistant\s+|model\s+)?(?:with(?:out)?|free\s+(?:of|from))\s+(?:no\s+|any\s+)?(?:restrictions?|rules|limits?|filters?|guidelines|ethical|ethics|safety)` +
			`|\b(?:without|with\s+no|free\s+(?:of|from))\s+(?:any\s+)?(?:ethical|moral|safety|content)\s+(?:restrictions?|guidelines|filters?|constraints?|limits?)\b`),
		literals: []string{"you are", " mode", "anything now", "restriction", "ethical", "moral", "safety", "content", "filter", "rules", "limit", "guideline"},
	},
	// Attempts to read the system prompt back.
	config.InjectionPromptLeak: {
		re: regexp.MustCompile(`(?i)\b(?:reveal|show|print|repeat|output|display|tell\s+me|give\s+me|write\s+out|leak|dump|disclose|recite|share)\b[^.\n]{0,40}\b(?:your|the)\s+(?:(?:full|entire|complete|exact|original|initial|hidden|secret|internal|above|previous)\s+){0,3}(?:system\s+(?:prompt|message|instructions?)|(?:initial|hidden|secret|original|internal)\s+(?:prompt|instructions?)|instructions?\s+(?:above|verbatim|word\s+for\s+word)|prompt\s+(?:above|verbatim|word\s+for\s+word))` +
			`|\brepeat\s+(?:everything|all(?:\s+the)?\s+(?:text|words))\s+(?:above|before\s+this)\b`),
		literals: []string{"prompt", "instruction", "system message", "everything above", "text above", "words above", "before this"},
	},
	// Chat template tokens and turn markers, which have no business in
	// user text or tool results.
	config.InjectionFakeTurn: {
		re: regexp.MustCompile(`<\|(?:im_start|im_end|system|user|assistant|endoftext|start_header_id|end_header_id|eot_id)\|>` +
			`|\[/?INST\]|<<\s*/?\s*SYS\s*>>|</?\s*system\s*>` +
			`|(?im)^\s*(?:#{2,}\s*)?(?:system|assistant)\s*(?:prompt|message|instruction)?s?\s*:\s*\S`),
		literals: []string{"<|", "inst]", "<<", "system", "assistant"},
	},
	// Characters a reader does not see but a model reads: Unicode tag
	// characters, bidirectional overrides, and runs of zero-width ones.
	config.InjectionHiddenText: {scan: hiddenText},

	// A link or image whose address carries text out: the classic way
	// an injected instruction sends a conversation to its author.
	config.InjectionExfiltration: {
		re: regexp.MustCompile(`(?i)!\[[^\]\n]{0,200}\]\(\s*https?://[^)\s]{1,500}[?&#][^)\s=]{0,60}=[^)\s]{24,}\)` +
			`|\b(?:send|post|forward|upload|exfiltrate|transmit|email)\b[^.\n]{0,60}\b(?:conversation|chat\s+history|system\s+prompt|api\s+keys?|credentials?|passwords?|secrets?|tokens?)\b[^.\n]{0,60}\bto\s+(?:https?://|[\w.+-]+@[\w-]+\.)`),
		literals: []string{"![", "send", "post", "forward", "upload", "exfiltrat", "transmit", "email"},
	},
}

// injection runs the selected injection checks and any extra patterns.
type injection struct {
	rules []injectionRule
}

func newInjection(g config.Guardrail) (*injection, error) {
	names := g.Detect
	if names == nil {
		names = config.InjectionChecks
	}
	c := &injection{}
	for _, n := range config.InjectionChecks {
		if slices.Contains(names, n) {
			r := injectionRules[n]
			r.name = n
			c.rules = append(c.rules, r)
		}
	}
	for _, n := range names {
		if _, ok := injectionRules[n]; !ok {
			return nil, fmt.Errorf("unknown injection check %q", n)
		}
	}
	for _, p := range g.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		c.rules = append(c.rules, injectionRule{name: "custom", re: re})
	}
	return c, nil
}

// asciiLower lowers ASCII letters only, so offsets stay those of s.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// How far a match may reach before and after one of its literals.
const (
	injReachBack    = 160
	injReachForward = 700
)

// count counts a rule's matches in t. With literals, the expression
// runs only on the text around each one, which keeps long harmless
// text cheap.
func (r *injectionRule) count(t, lower string) int {
	if r.scan != nil {
		return r.scan(t)
	}
	if r.literals == nil {
		return len(r.re.FindAllStringIndex(t, 20))
	}
	var spans [][2]int
	for _, l := range r.literals {
		for from := 0; ; {
			i := strings.Index(lower[from:], l)
			if i < 0 {
				break
			}
			at := from + i
			spans = append(spans, [2]int{max(0, at-injReachBack), min(len(t), at+injReachForward)})
			from = at + len(l)
		}
	}
	if len(spans) == 0 {
		return 0
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	n, start, end := 0, spans[0][0], spans[0][1]
	flush := func() {
		// Do not start or stop inside a UTF-8 sequence.
		for start > 0 && t[start]&0xC0 == 0x80 {
			start--
		}
		for end < len(t) && t[end]&0xC0 == 0x80 {
			end++
		}
		n += len(r.re.FindAllStringIndex(t[start:end], 20))
	}
	for _, sp := range spans[1:] {
		if sp[0] <= end {
			end = max(end, sp[1])
			continue
		}
		flush()
		start, end = sp[0], sp[1]
	}
	flush()
	return n
}

func (c *injection) check(_ context.Context, _ Meta, _ string, texts []string) (verdict, error) {
	counts := map[string]int{}
	for _, t := range texts {
		if t == "" {
			continue
		}
		lower := ""
		for i := range c.rules {
			r := &c.rules[i]
			if r.literals != nil && lower == "" {
				lower = asciiLower(t)
			}
			if n := r.count(t, lower); n > 0 {
				counts[r.name] += n
			}
		}
	}
	if len(counts) == 0 {
		return verdict{}, nil
	}
	found := make([]string, 0, len(counts))
	for n := range counts {
		found = append(found, n)
	}
	slices.Sort(found)
	return verdict{match: true, counts: counts, reason: "looks like prompt injection: " + strings.Join(found, ", ")}, nil
}
