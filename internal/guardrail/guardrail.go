// Package guardrail checks the text of model and MCP calls against the
// guardrails in config (docs/guardrails.md).
package guardrail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/redact"
)

// Meta identifies the request being checked.
type Meta struct {
	RequestID, OrgID, TeamID, KeyID string
	// Value is passed to Classify unchanged; the server puts the
	// principal there, to meter classifier calls.
	Value any
}

// Classify asks a virtual model about text, with an optional system
// prompt, and returns the model's reply.
type Classify func(ctx context.Context, meta Meta, model, system, text string) (string, error)

// Decision is what one guardrail did at one stage. Only guardrails that
// matched or failed produce a decision.
type Decision struct {
	Guardrail string         `json:"guardrail"`
	Stage     string         `json:"stage"`
	Action    string         `json:"action"`
	Reason    string         `json:"reason,omitempty"`
	Counts    map[string]int `json:"counts,omitempty"`
	// Failed is set when the check itself failed; Action is then what
	// on_error chose.
	Failed  bool          `json:"failed,omitempty"`
	Latency time.Duration `json:"-"`
}

// Result is the outcome of the guardrails at one stage.
type Result struct {
	// Texts are the checked texts, redacted where a guardrail redacted.
	Texts   []string
	Changed bool
	// Blocked is the decision that blocked the request, or nil.
	Blocked   *Decision
	Decisions []Decision
}

// Observer is told about every check, for metrics.
type Observer func(guardrail, stage, result string, d time.Duration)

// Check results reported to the Observer.
const (
	ResultPass  = "pass"
	ResultMatch = "match"
	ResultError = "error"
)

// verdict is what a check found.
type verdict struct {
	match bool
	// action, when set, is the action a webhook asked for.
	action string
	texts  []string // replacements, when the check redacts
	counts map[string]int
	reason string
}

type checker interface {
	check(ctx context.Context, meta Meta, stage string, texts []string) (verdict, error)
}

// guard is one configured guardrail.
type guard struct {
	name string
	cfg  config.Guardrail
	checker
}

// action is what the guardrail does when it matches: its configured
// action, or for a webhook not in log mode, what the webhook asked for.
func (g *guard) action(v verdict) string {
	if g.cfg.Action == config.ActionLog || v.action == "" {
		return g.cfg.Action
	}
	return v.action
}

// mayRedact reports whether the guardrail can change the texts.
func (g *guard) mayRedact() bool {
	if g.cfg.Type == config.GuardrailWebhook {
		return g.cfg.Action != config.ActionLog
	}
	return g.cfg.Action == config.ActionRedact
}

// Set holds the configured guardrails.
type Set struct {
	guards  map[string]*guard
	observe Observer
}

// New builds the guardrails in config. client sends webhook requests and
// classify calls classifier models.
func New(cfg map[string]config.Guardrail, client *http.Client, version string, classify Classify, observe Observer) (*Set, error) {
	s := &Set{guards: map[string]*guard{}, observe: observe}
	if s.observe == nil {
		s.observe = func(string, string, string, time.Duration) {}
	}
	for name, g := range cfg {
		var c checker
		switch g.Type {
		case config.GuardrailPII, config.GuardrailRegex:
			rc := redact.Config{Builtin: g.Detect}
			if g.Type == config.GuardrailRegex {
				rc = redact.Config{Builtin: []string{}, Patterns: g.Patterns}
			}
			r, err := redact.New(rc)
			if err != nil {
				return nil, fmt.Errorf("guardrail %q: %w", name, err)
			}
			c = &patterns{r: r}
		case config.GuardrailModel:
			re, err := regexp.Compile(g.UnsafeIf)
			if err != nil {
				return nil, fmt.Errorf("guardrail %q: %w", name, err)
			}
			c = &classifier{model: g.Model, system: g.Prompt, unsafe: re, classify: classify}
		case config.GuardrailWebhook:
			c = &webhook{name: name, url: g.URL.Value(), secret: g.Secret.Value(), client: client, version: version}
		default:
			return nil, fmt.Errorf("guardrail %q: unknown type %q", name, g.Type)
		}
		s.guards[name] = &guard{name: name, cfg: g, checker: c}
	}
	return s, nil
}

// Has reports whether a guardrail is configured.
func (s *Set) Has(name string) bool {
	_, ok := s.guards[name]
	return ok
}

// Applies reports whether any of the named guardrails runs at a stage.
func (s *Set) Applies(stage string, names []string) bool {
	return len(s.pick(stage, names)) > 0
}

// Names returns the names of the guardrails that run at a stage, in the
// order they run in.
func (s *Set) Names(stage string, names []string) []string {
	var out []string
	for _, g := range s.pick(stage, names) {
		out = append(out, g.name)
	}
	return out
}

// pick returns the named guardrails that run at a stage, in name order.
// Names not in config are skipped: a policy can outlive a guardrail.
func (s *Set) pick(stage string, names []string) []*guard {
	var out []*guard
	for _, n := range names {
		if g := s.guards[n]; g != nil && slices.Contains(g.cfg.Stages, stage) && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b *guard) int { return strings.Compare(a.name, b.name) })
	return out
}

// Run applies the named guardrails at a stage to texts. In-process
// checks run first, in name order. External checks that only block or
// log then run in parallel, and webhooks that may redact run last, in
// order, each on the texts the previous one returned.
func (s *Set) Run(ctx context.Context, stage string, names []string, texts []string, meta Meta) Result {
	res := Result{Texts: texts}
	guards := s.pick(stage, names)
	if len(guards) == 0 || len(texts) == 0 {
		return res
	}
	var local, parallel, redacting []*guard
	for _, g := range guards {
		switch {
		case !g.cfg.External():
			local = append(local, g)
		case g.mayRedact():
			redacting = append(redacting, g)
		default:
			parallel = append(parallel, g)
		}
	}
	for _, g := range local {
		if s.apply(&res, g, stage, s.run(ctx, g, stage, res.Texts, meta)) {
			return res
		}
	}
	outcomes := make([]outcome, len(parallel))
	var wg sync.WaitGroup
	for i, g := range parallel {
		wg.Go(func() { outcomes[i] = s.run(ctx, g, stage, res.Texts, meta) })
	}
	wg.Wait()
	for i, g := range parallel {
		if s.apply(&res, g, stage, outcomes[i]) {
			return res
		}
	}
	for _, g := range redacting {
		if s.apply(&res, g, stage, s.run(ctx, g, stage, res.Texts, meta)) {
			return res
		}
	}
	return res
}

type outcome struct {
	v       verdict
	err     error
	latency time.Duration
}

func (s *Set) run(ctx context.Context, g *guard, stage string, texts []string, meta Meta) outcome {
	start := time.Now()
	if g.cfg.External() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.cfg.Timeout.Std())
		defer cancel()
	}
	v, err := g.check(ctx, meta, stage, texts)
	o := outcome{v: v, err: err, latency: time.Since(start)}
	result := ResultPass
	switch {
	case err != nil:
		result = ResultError
	case v.match:
		result = ResultMatch
	}
	s.observe(g.name, stage, result, o.latency)
	return o
}

// apply records one guardrail's outcome and reports whether it blocked.
func (s *Set) apply(res *Result, g *guard, stage string, o outcome) bool {
	d := Decision{Guardrail: g.name, Stage: stage, Action: g.action(o.v), Counts: o.v.counts, Reason: o.v.reason, Latency: o.latency}
	switch {
	case o.err != nil:
		d.Failed, d.Reason = true, errorReason(o.err)
		d.Action = config.ActionLog
		if g.cfg.OnError == config.OnErrorBlock {
			d.Action = config.ActionBlock
		}
	case !o.v.match:
		return false
	}
	switch d.Action {
	case config.ActionBlock:
		res.Decisions = append(res.Decisions, d)
		res.Blocked = &res.Decisions[len(res.Decisions)-1]
		return true
	case config.ActionRedact:
		if len(o.v.texts) == len(res.Texts) {
			res.Texts, res.Changed = o.v.texts, true
		}
	}
	res.Decisions = append(res.Decisions, d)
	return false
}

func errorReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "check timed out"
	}
	return truncate("check failed: "+err.Error(), maxReason)
}

// maxReason bounds a stored reason.
const maxReason = 200

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// patterns runs the redactor's detectors: pii and regex guardrails.
type patterns struct{ r *redact.Redactor }

func (p *patterns) check(_ context.Context, _ Meta, _ string, texts []string) (verdict, error) {
	counts := redact.Counts{}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = p.r.String(t, counts)
	}
	if len(counts) == 0 {
		return verdict{}, nil
	}
	return verdict{match: true, texts: out, counts: counts}, nil
}

// classifier asks a model whether the text is unsafe.
type classifier struct {
	model, system string
	unsafe        *regexp.Regexp
	classify      Classify
}

func (c *classifier) check(ctx context.Context, meta Meta, _ string, texts []string) (verdict, error) {
	if c.classify == nil {
		return verdict{}, errors.New("no classifier")
	}
	reply, err := c.classify(ctx, meta, c.model, c.system, strings.Join(texts, "\n\n"))
	if err != nil {
		return verdict{}, err
	}
	if !c.unsafe.MatchString(reply) {
		return verdict{}, nil
	}
	return verdict{match: true, reason: truncate(strings.Join(strings.Fields(reply), " "), maxReason)}, nil
}
