// Package limits enforces requests-per-minute, tokens-per-minute and
// budget limits at the org, team and key levels. See ADR 0005.
package limits

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/policy"
)

// Rejection codes, matching docs/api.md.
const (
	CodeRateLimited = "rate_limit_exceeded"
	CodeBudget      = "budget_exceeded"
	CodeUnavailable = "limiter_unavailable"
)

// Decision is the outcome of Admit.
type Decision struct {
	Allowed    bool
	Code       string
	Message    string
	RetryAfter time.Duration
	// Requests and Tokens describe the tightest per-minute limits that
	// apply, for rate limit headers. Limit is zero when none applies.
	Requests, Tokens Window
}

// Window is a per-minute limit and what is left of it.
type Window struct {
	Limit, Remaining int64
	Reset            time.Duration
}

// Limiter checks and charges limits against a backend.
type Limiter struct {
	b              Backend
	failOpenRate   bool
	failOpenBudget bool
	now            func() time.Time
}

// New returns a limiter. failOpenRate and failOpenBudget decide whether
// requests pass when the backend is unreachable.
func New(b Backend, failOpenRate, failOpenBudget bool) *Limiter {
	return &Limiter{b: b, failOpenRate: failOpenRate, failOpenBudget: failOpenBudget, now: time.Now}
}

var levelNames = [3]string{"organization", "team", "key"}

func scopes(p *auth.Principal) [3]string { return [3]string{p.OrgID, p.TeamID, p.KeyID} }

func minuteKey(kind, scope string, window time.Time) string {
	return "gog:" + kind + ":" + scope + ":" + strconv.FormatInt(window.Unix(), 10)
}

// spendKey names the counter for a scope's spend in the period holding t,
// and returns how long the counter must live.
func spendKey(scope, period string, t time.Time) (string, time.Duration) {
	t = t.UTC()
	switch period {
	case policy.PeriodDaily:
		end := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
		return "gog:spend:" + scope + ":" + t.Format("20060102"), end.Sub(t) + 24*time.Hour
	case policy.PeriodMonthly:
		end := time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		return "gog:spend:" + scope + ":" + t.Format("200601"), end.Sub(t) + 24*time.Hour
	}
	return "gog:spend:" + scope + ":total", 0
}

// PeriodStart returns when the budget period holding t began.
func PeriodStart(period string, t time.Time) time.Time {
	t = t.UTC()
	switch period {
	case policy.PeriodDaily:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	case policy.PeriodMonthly:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Unix(0, 0).UTC()
}

// Admit counts a request against every request limit and checks token
// and budget limits. It must be called once per request.
func (l *Limiter) Admit(ctx context.Context, p *auth.Principal) Decision {
	now := l.now()
	window := now.Truncate(time.Minute)
	reset := window.Add(time.Minute).Sub(now)
	ids := scopes(p)
	d := Decision{Allowed: true}

	for i, pol := range p.Policies {
		if pol.RPM > 0 {
			n, err := l.b.Add(ctx, minuteKey("rpm", ids[i], window), 1, 2*time.Minute)
			if err != nil {
				return l.unavailable(d, l.failOpenRate, err)
			}
			d.Requests = tighter(d.Requests, Window{pol.RPM, pol.RPM - int64(n), reset})
			if int64(n) > pol.RPM {
				return reject(d, CodeRateLimited, fmt.Sprintf("%s request limit of %d per minute reached", levelNames[i], pol.RPM), reset)
			}
		}
		if pol.TPM > 0 {
			v, err := l.b.Get(ctx, minuteKey("tpm", ids[i], window))
			if err != nil {
				return l.unavailable(d, l.failOpenRate, err)
			}
			d.Tokens = tighter(d.Tokens, Window{pol.TPM, pol.TPM - int64(v[0]), reset})
			if int64(v[0]) >= pol.TPM {
				return reject(d, CodeRateLimited, fmt.Sprintf("%s token limit of %d per minute reached", levelNames[i], pol.TPM), reset)
			}
		}
		if b := pol.Budget; b != nil {
			key, _ := spendKey(ids[i], b.Period, now)
			v, err := l.b.Get(ctx, key)
			if err != nil {
				return l.unavailable(d, l.failOpenBudget, err)
			}
			if v[0] >= b.AmountUSD {
				return reject(d, CodeBudget, fmt.Sprintf("%s %s budget of $%.2f is spent", levelNames[i], b.Period, b.AmountUSD), 0)
			}
		}
	}
	return d
}

// Charge records tokens and cost after a request completes. Errors are
// returned for logging; the request has already been served.
func (l *Limiter) Charge(ctx context.Context, p *auth.Principal, tokens int64, costUSD float64) error {
	now := l.now()
	window := now.Truncate(time.Minute)
	ids := scopes(p)
	for i, pol := range p.Policies {
		if pol.TPM > 0 && tokens > 0 {
			if _, err := l.b.Add(ctx, minuteKey("tpm", ids[i], window), float64(tokens), 2*time.Minute); err != nil {
				return err
			}
		}
		if pol.Budget != nil && costUSD > 0 {
			key, ttl := spendKey(ids[i], pol.Budget.Period, now)
			if _, err := l.b.Add(ctx, key, costUSD, ttl); err != nil {
				return err
			}
		}
	}
	return nil
}

// Seed raises spend counters to recorded spend so a restarted memory
// backend or a flushed Valkey does not reset budgets. spend maps scope
// ids to USD spent since the start of the period.
func (l *Limiter) Seed(ctx context.Context, period string, spend map[string]float64) error {
	now := l.now()
	for scope, v := range spend {
		key, ttl := spendKey(scope, period, now)
		if err := l.b.Raise(ctx, key, v, ttl); err != nil {
			return err
		}
	}
	return nil
}

func (l *Limiter) unavailable(d Decision, failOpen bool, err error) Decision {
	if failOpen {
		return d
	}
	return Decision{Code: CodeUnavailable, Message: "limits could not be checked: " + err.Error()}
}

func reject(d Decision, code, msg string, retry time.Duration) Decision {
	d.Allowed, d.Code, d.Message = false, code, msg
	d.RetryAfter = retry
	return d
}

// tighter returns the window with less remaining.
func tighter(a, b Window) Window {
	if b.Remaining < 0 {
		b.Remaining = 0
	}
	if a.Limit == 0 || b.Remaining < a.Remaining {
		return b
	}
	return a
}

// RetryAfterSeconds rounds a duration up to whole seconds for the
// retry-after header.
func RetryAfterSeconds(d time.Duration) int64 { return int64(math.Ceil(d.Seconds())) }
