package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Daily usage rollups (docs/analytics.md).

const dayMS = 86400000

// DayStart is the start of t's UTC day.
func DayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

const rollupDims = `org_id, team_id, key_id, endpoint, model, provider, upstream_model`

const rollupSums = `requests, errors, input_tokens, output_tokens, cached_input_tokens, reasoning_tokens, images, characters,
	audio_seconds, video_seconds, cost_usd, cache_hits, saved_usd, latency_ms_sum`

// RollupUsage rebuilds the daily rollups of the UTC days from from's
// day up to, not including, to's day, from the usage rows. A day with
// no usage rows keeps the rollups it has: its rows were pruned, and
// rows are pruned by whole days.
func (s *Store) RollupUsage(ctx context.Context, from, to time.Time) error {
	fromDay, toDay := DayStart(from).UnixMilli()/dayMS, DayStart(to).UnixMilli()/dayMS
	if toDay <= fromDay {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM usage_daily WHERE day >= ? AND day < ?
			AND day IN (SELECT DISTINCT ts / 86400000 FROM usage WHERE ts >= ? AND ts < ?)`), fromDay, toDay, fromDay*dayMS, toDay*dayMS); err != nil {
			return err
		}
		// Another replica rebuilding the same days wrote the same sums.
		_, err := tx.ExecContext(ctx, s.q(`INSERT INTO usage_daily (day, `+rollupDims+`, `+rollupSums+`)
			SELECT ts / 86400000, `+rollupDims+`, COUNT(*), COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0),
				COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cached_input_tokens), 0), COALESCE(SUM(reasoning_tokens), 0),
				COALESCE(SUM(images), 0), COALESCE(SUM(characters), 0), COALESCE(SUM(audio_seconds), 0), COALESCE(SUM(video_seconds), 0),
				COALESCE(SUM(cost_usd), 0), COALESCE(SUM(cache_hit), 0), COALESCE(SUM(saved_usd), 0), COALESCE(SUM(latency_ms), 0)
			FROM usage WHERE ts >= ? AND ts < ?
			GROUP BY ts / 86400000, `+rollupDims+`
			ON CONFLICT DO NOTHING`), fromDay*dayMS, toDay*dayMS)
		return err
	})
}

// RollupBounds returns the last day that has rollups and the first day
// that has usage rows. Each is zero when its table is empty.
func (s *Store) RollupBounds(ctx context.Context) (lastRollup, firstUsage time.Time, err error) {
	var d, ts sql.NullInt64
	if err = s.db.QueryRowContext(ctx, `SELECT MAX(day) FROM usage_daily`).Scan(&d); err != nil {
		return
	}
	if err = s.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM usage`).Scan(&ts); err != nil {
		return
	}
	if d.Valid {
		lastRollup = toTime(d.Int64 * dayMS)
	}
	if ts.Valid {
		firstUsage = DayStart(toTime(ts.Int64))
	}
	return
}

// PruneRollups deletes rollups of days before a time.
func (s *Store) PruneRollups(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM usage_daily WHERE day < ?`), DayStart(before).UnixMilli()/dayMS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// usageSource is the FROM clause of a usage report: the usage rows, and
// for a query that reaches back before RawFrom, the rollups of the days
// before it. It returns the subquery, aliased u, and its arguments.
// dims are the columns the report groups or filters by; day is added.
func (s *Store) usageSource(uq UsageQuery) (string, []any) {
	filter := func(args []any) (string, []any) {
		var w []string
		for _, f := range [][2]string{{"org_id", uq.OrgID}, {"team_id", uq.TeamID}, {"key_id", uq.KeyID}} {
			if f[1] != "" {
				w = append(w, " AND "+f[0]+" = ?")
				args = append(args, f[1])
			}
		}
		return strings.Join(w, ""), args
	}
	rawFrom := uq.From
	rolled := !uq.RawFrom.IsZero() && uq.From.Before(uq.RawFrom)
	if rolled {
		rawFrom = uq.RawFrom
	}
	where, args := filter([]any{rawFrom.UnixMilli(), uq.To.UnixMilli()})
	q := `SELECT ts / 86400000 AS day, ` + rollupDims + `, 1 AS requests, input_tokens, output_tokens, cached_input_tokens, reasoning_tokens,
		images, characters, audio_seconds, video_seconds, cost_usd, cache_hit AS cache_hits, saved_usd
		FROM usage WHERE ts >= ? AND ts < ?` + where
	if rolled {
		// Rollups are whole days: the first day counts in full.
		toDay := min(uq.RawFrom.UnixMilli(), uq.To.UnixMilli()+dayMS-1) / dayMS
		var w string
		w, args = filter(append(args, DayStart(uq.From).UnixMilli()/dayMS, toDay))
		q += ` UNION ALL SELECT day, ` + rollupDims + `, requests, input_tokens, output_tokens, cached_input_tokens, reasoning_tokens,
			images, characters, audio_seconds, video_seconds, cost_usd, cache_hits, saved_usd
			FROM usage_daily WHERE day >= ? AND day < ?` + w
	}
	return `(` + q + `) AS u`, args
}

const usageSums = `COALESCE(SUM(requests), 0), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(cached_input_tokens), 0), COALESCE(SUM(reasoning_tokens), 0), COALESCE(SUM(images), 0), COALESCE(SUM(characters), 0),
	COALESCE(SUM(audio_seconds), 0), COALESCE(SUM(video_seconds), 0), COALESCE(SUM(cost_usd), 0),
	COALESCE(SUM(cache_hits), 0), COALESCE(SUM(saved_usd), 0)`

// Rollup is one row of the daily rollups.
type Rollup struct {
	Day           string `json:"day"` // UTC, as 2006-01-02
	OrgID         string `json:"org_id"`
	TeamID        string `json:"team_id"`
	KeyID         string `json:"key_id"`
	Endpoint      string `json:"endpoint"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	Requests      int64  `json:"requests"`
	Errors        int64  `json:"errors"`
	UsageTotals
	LatencyMSSum int64 `json:"latency_ms_sum"`
}

// UsageTotals are the summed amounts of a rollup.
type UsageTotals struct {
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CachedInputTokens int64   `json:"cached_input_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	Images            int64   `json:"images"`
	Characters        int64   `json:"characters"`
	AudioSeconds      float64 `json:"audio_seconds"`
	VideoSeconds      float64 `json:"video_seconds"`
	CostUSD           float64 `json:"cost_usd"`
	CacheHits         int64   `json:"cache_hits"`
	SavedUSD          float64 `json:"saved_usd"`
}

// EachRollup calls fn with every rollup of the UTC days from From's day
// up to, not including, To's day, in day order.
func (s *Store) EachRollup(ctx context.Context, uq UsageQuery, fn func(*Rollup) error) error {
	where := []string{"day >= ?", "day < ?"}
	args := []any{DayStart(uq.From).UnixMilli() / dayMS, (uq.To.UnixMilli() + dayMS - 1) / dayMS}
	for _, f := range [][2]string{{"org_id", uq.OrgID}, {"team_id", uq.TeamID}, {"key_id", uq.KeyID}} {
		if f[1] != "" {
			where = append(where, f[0]+" = ?")
			args = append(args, f[1])
		}
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT day, `+rollupDims+`, `+rollupSums+` FROM usage_daily
		WHERE `+strings.Join(where, " AND ")+` ORDER BY day, `+rollupDims), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r Rollup
		var day int64
		if err := rows.Scan(&day, &r.OrgID, &r.TeamID, &r.KeyID, &r.Endpoint, &r.Model, &r.Provider, &r.UpstreamModel,
			&r.Requests, &r.Errors, &r.InputTokens, &r.OutputTokens, &r.CachedInputTokens, &r.ReasoningTokens, &r.Images, &r.Characters,
			&r.AudioSeconds, &r.VideoSeconds, &r.CostUSD, &r.CacheHits, &r.SavedUSD, &r.LatencyMSSum); err != nil {
			return err
		}
		r.Day = toTime(day * dayMS).Format("2006-01-02")
		if err := fn(&r); err != nil {
			return err
		}
	}
	return rows.Err()
}
