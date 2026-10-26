// Package store persists tenants, keys, usage and the audit log in SQLite
// or Postgres. The same SQL runs on both; see ADR 0004.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver

	"github.com/rnikadi/genaiopengateway/internal/id"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Errors returned by store methods.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Org is an organization.
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	policy.Policy
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Team belongs to one organization.
type Team struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	Name  string `json:"name"`
	policy.Policy
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Key is an API key record. The secret itself is never stored.
type Key struct {
	ID         string `json:"id"`
	OrgID      string `json:"org_id"`
	TeamID     string `json:"team_id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	SecretHash string `json:"-"`
	// Owner is who the key belongs to: a person's email or a service.
	Owner string `json:"owner,omitempty"`
	policy.Policy
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// PreviousHash is the secret a rotation replaced, which works until
	// PreviousExpiresAt.
	PreviousHash      string     `json:"-"`
	PreviousExpiresAt *time.Time `json:"previous_expires_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt        *time.Time `json:"last_used_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// Page selects a page of a list ordered by id.
type Page struct {
	Limit int
	After string
}

// Store is a SQL database holding gateway state.
type Store struct {
	db       *sql.DB
	postgres bool
}

// Open connects to the database and applies pending migrations.
func Open(ctx context.Context, driver, dsn string) (*Store, error) {
	name := map[string]string{"sqlite": "sqlite", "postgres": "pgx"}[driver]
	if name == "" {
		return nil, fmt.Errorf("unknown store driver %q", driver)
	}
	db, err := sql.Open(name, dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, postgres: driver == "postgres"}
	if !s.postgres {
		// SQLite allows one writer; a single connection avoids busy errors
		// and keeps pragmas per connection consistent.
		db.SetMaxOpenConns(1)
		for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
			if _, err := db.ExecContext(ctx, pragma); err != nil {
				db.Close()
				return nil, fmt.Errorf("sqlite %s: %w", pragma, err)
			}
		}
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the connection.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// q rewrites ? placeholders to $n for Postgres.
func (s *Store) q(query string) string {
	if !s.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// migrationLock is the Postgres advisory lock key that serializes
// migrations across replicas starting together.
const migrationLock = 0x676f675f6d6967 // "gog_mig"

func (s *Store) migrate(ctx context.Context) error {
	if s.postgres {
		// One replica migrates; the others wait here, then find nothing
		// left to do. The lock belongs to this connection and ends with it.
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return err
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
			return fmt.Errorf("migration lock: %w", err)
		}
		defer func() {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLock)
		}()
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		version := strings.TrimSuffix(strings.TrimPrefix(f, "migrations/"), ".sql")
		var n int
		if err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`), version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, _ := migrations.ReadFile(f)
		err := s.tx(ctx, func(tx *sql.Tx) error {
			for _, stmt := range strings.Split(string(body), ";") {
				if strings.TrimSpace(stripComments(stmt)) == "" {
					continue
				}
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, s.q(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`), version, now())
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", version, err)
		}
	}
	return nil
}

func stripComments(stmt string) string {
	var out []string
	for _, line := range strings.Split(stmt, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func (s *Store) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func now() int64 { return time.Now().UnixMilli() }

func toTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func toTimePtr(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}
	t := toTime(ms.Int64)
	return &t
}

func fromTimePtr(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func encodePolicy(p policy.Policy) string {
	b, _ := json.Marshal(p)
	return string(b)
}

func decodePolicy(raw string) (policy.Policy, error) {
	var p policy.Policy
	err := json.Unmarshal([]byte(raw), &p)
	return p, err
}

func limit(p Page) int {
	if p.Limit <= 0 {
		return 50
	}
	return min(p.Limit, 500)
}

// --- organizations ---

// CreateOrg inserts an organization, assigning its id and times.
func (s *Store) CreateOrg(ctx context.Context, o *Org) error {
	o.ID = id.New("org")
	o.CreatedAt = toTime(now())
	o.UpdatedAt = o.CreatedAt
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO orgs (id, name, policy, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`),
		o.ID, o.Name, encodePolicy(o.Policy), o.CreatedAt.UnixMilli(), o.UpdatedAt.UnixMilli())
	return err
}

const orgCols = `id, name, policy, created_at, updated_at`

func scanOrg(row interface{ Scan(...any) error }) (*Org, error) {
	var o Org
	var pol string
	var created, updated int64
	if err := row.Scan(&o.ID, &o.Name, &pol, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	o.Policy, err = decodePolicy(pol)
	o.CreatedAt, o.UpdatedAt = toTime(created), toTime(updated)
	return &o, err
}

// GetOrg returns one organization.
func (s *Store) GetOrg(ctx context.Context, orgID string) (*Org, error) {
	o, err := scanOrg(s.db.QueryRowContext(ctx, s.q(`SELECT `+orgCols+` FROM orgs WHERE id = ?`), orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}

// ListOrgs returns a page of organizations and whether more exist.
func (s *Store) ListOrgs(ctx context.Context, p Page) ([]*Org, bool, error) {
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+orgCols+` FROM orgs WHERE id > ? ORDER BY id LIMIT ?`), p.After, n+1)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanOrg)
}

// UpdateOrg saves the name and policy of an organization.
func (s *Store) UpdateOrg(ctx context.Context, o *Org) error {
	o.UpdatedAt = toTime(now())
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE orgs SET name = ?, policy = ?, updated_at = ? WHERE id = ?`),
		o.Name, encodePolicy(o.Policy), o.UpdatedAt.UnixMilli(), o.ID)
	return affected(res, err)
}

// DeleteOrg removes an organization that has no teams.
func (s *Store) DeleteOrg(ctx context.Context, orgID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM teams WHERE org_id = ?`), orgID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: organization has %d teams", ErrConflict, n)
		}
		return affected(tx.ExecContext(ctx, s.q(`DELETE FROM orgs WHERE id = ?`), orgID))
	})
}

// --- teams ---

// CreateTeam inserts a team into an existing organization.
func (s *Store) CreateTeam(ctx context.Context, t *Team) error {
	if _, err := s.GetOrg(ctx, t.OrgID); err != nil {
		return err
	}
	t.ID = id.New("team")
	t.CreatedAt = toTime(now())
	t.UpdatedAt = t.CreatedAt
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO teams (id, org_id, name, policy, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`),
		t.ID, t.OrgID, t.Name, encodePolicy(t.Policy), t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	return err
}

const teamCols = `id, org_id, name, policy, created_at, updated_at`

func scanTeam(row interface{ Scan(...any) error }) (*Team, error) {
	var t Team
	var pol string
	var created, updated int64
	if err := row.Scan(&t.ID, &t.OrgID, &t.Name, &pol, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	t.Policy, err = decodePolicy(pol)
	t.CreatedAt, t.UpdatedAt = toTime(created), toTime(updated)
	return &t, err
}

// GetTeam returns one team.
func (s *Store) GetTeam(ctx context.Context, teamID string) (*Team, error) {
	t, err := scanTeam(s.db.QueryRowContext(ctx, s.q(`SELECT `+teamCols+` FROM teams WHERE id = ?`), teamID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ListTeams returns a page of an organization's teams.
func (s *Store) ListTeams(ctx context.Context, orgID string, p Page) ([]*Team, bool, error) {
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+teamCols+` FROM teams WHERE org_id = ? AND id > ? ORDER BY id LIMIT ?`), orgID, p.After, n+1)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanTeam)
}

// UpdateTeam saves the name and policy of a team.
func (s *Store) UpdateTeam(ctx context.Context, t *Team) error {
	t.UpdatedAt = toTime(now())
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE teams SET name = ?, policy = ?, updated_at = ? WHERE id = ?`),
		t.Name, encodePolicy(t.Policy), t.UpdatedAt.UnixMilli(), t.ID)
	return affected(res, err)
}

// DeleteTeam removes a team with no active keys, and its revoked keys.
func (s *Store) DeleteTeam(ctx context.Context, teamID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM api_keys WHERE team_id = ? AND revoked_at IS NULL`), teamID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: team has %d active keys", ErrConflict, n)
		}
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM api_keys WHERE team_id = ?`), teamID); err != nil {
			return err
		}
		return affected(tx.ExecContext(ctx, s.q(`DELETE FROM teams WHERE id = ?`), teamID))
	})
}

// --- keys ---

// CreateKey inserts a key for an existing team. SecretHash and Prefix
// must be set by the caller.
func (s *Store) CreateKey(ctx context.Context, k *Key) error {
	t, err := s.GetTeam(ctx, k.TeamID)
	if err != nil {
		return err
	}
	k.ID = id.New("key")
	k.OrgID = t.OrgID
	k.CreatedAt = toTime(now())
	k.UpdatedAt = k.CreatedAt
	_, err = s.db.ExecContext(ctx, s.q(`INSERT INTO api_keys (id, org_id, team_id, name, prefix, secret_hash, owner, policy, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		k.ID, k.OrgID, k.TeamID, k.Name, k.Prefix, k.SecretHash, k.Owner, encodePolicy(k.Policy), fromTimePtr(k.ExpiresAt), k.CreatedAt.UnixMilli(), k.UpdatedAt.UnixMilli())
	return err
}

const keyCols = `id, org_id, team_id, name, prefix, secret_hash, owner, policy, expires_at, previous_hash, previous_expires_at, revoked_at, last_used_at, created_at, updated_at`

func scanKey(row interface{ Scan(...any) error }) (*Key, error) {
	var k Key
	var pol string
	var expires, prevExpires, revoked, lastUsed sql.NullInt64
	var created, updated int64
	if err := row.Scan(&k.ID, &k.OrgID, &k.TeamID, &k.Name, &k.Prefix, &k.SecretHash, &k.Owner, &pol, &expires, &k.PreviousHash, &prevExpires, &revoked, &lastUsed, &created, &updated); err != nil {
		return nil, err
	}
	var err error
	k.Policy, err = decodePolicy(pol)
	k.ExpiresAt, k.RevokedAt, k.LastUsedAt = toTimePtr(expires), toTimePtr(revoked), toTimePtr(lastUsed)
	k.PreviousExpiresAt = toTimePtr(prevExpires)
	k.CreatedAt, k.UpdatedAt = toTime(created), toTime(updated)
	return &k, err
}

// GetKey returns one key.
func (s *Store) GetKey(ctx context.Context, keyID string) (*Key, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx, s.q(`SELECT `+keyCols+` FROM api_keys WHERE id = ?`), keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// ListKeys returns a page of a team's keys, including revoked ones.
func (s *Store) ListKeys(ctx context.Context, teamID string, p Page) ([]*Key, bool, error) {
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+keyCols+` FROM api_keys WHERE team_id = ? AND id > ? ORDER BY id LIMIT ?`), teamID, p.After, n+1)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanKey)
}

// KeyFilter selects keys across teams. Empty fields match everything.
type KeyFilter struct {
	// OrgIDs limits the keys to these organizations; nil means all.
	OrgIDs []string
	OrgID  string
	TeamID string
	// Query matches the name, prefix or owner, ignoring case.
	Query string
	// Status is active, expiring (within 7 days), expired, revoked or
	// rotating (a previous secret still works).
	Status string
}

// KeyStatuses are the values of KeyFilter.Status.
var KeyStatuses = []string{"active", "expiring", "expired", "revoked", "rotating"}

// ListAllKeys returns a page of keys across teams, including revoked ones.
func (s *Store) ListAllKeys(ctx context.Context, f KeyFilter, p Page) ([]*Key, bool, error) {
	if f.OrgIDs != nil && len(f.OrgIDs) == 0 {
		return nil, false, nil
	}
	where, args := []string{"id > ?"}, []any{p.After}
	if f.OrgIDs != nil {
		where = append(where, "org_id IN (?"+strings.Repeat(", ?", len(f.OrgIDs)-1)+")")
		for _, o := range f.OrgIDs {
			args = append(args, o)
		}
	}
	if f.OrgID != "" {
		where, args = append(where, "org_id = ?"), append(args, f.OrgID)
	}
	if f.TeamID != "" {
		where, args = append(where, "team_id = ?"), append(args, f.TeamID)
	}
	if f.Query != "" {
		q := "%" + strings.ToLower(f.Query) + "%"
		where, args = append(where, "(LOWER(name) LIKE ? OR LOWER(prefix) LIKE ? OR LOWER(owner) LIKE ?)"), append(args, q, q, q)
	}
	t := now()
	switch f.Status {
	case "active":
		where, args = append(where, "revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)"), append(args, t)
	case "expiring":
		where, args = append(where, "revoked_at IS NULL AND expires_at > ? AND expires_at <= ?"), append(args, t, t+7*24*time.Hour.Milliseconds())
	case "expired":
		where, args = append(where, "revoked_at IS NULL AND expires_at <= ?"), append(args, t)
	case "revoked":
		where = append(where, "revoked_at IS NOT NULL")
	case "rotating":
		where, args = append(where, "revoked_at IS NULL AND previous_expires_at > ?"), append(args, t)
	}
	n := limit(p)
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+keyCols+` FROM api_keys WHERE `+strings.Join(where, " AND ")+` ORDER BY id LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, scanKey)
}

// UpdateKey saves a key's name, owner, policy and expiry.
func (s *Store) UpdateKey(ctx context.Context, k *Key) error {
	k.UpdatedAt = toTime(now())
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE api_keys SET name = ?, owner = ?, policy = ?, expires_at = ?, updated_at = ? WHERE id = ?`),
		k.Name, k.Owner, encodePolicy(k.Policy), fromTimePtr(k.ExpiresAt), k.UpdatedAt.UnixMilli(), k.ID)
	return affected(res, err)
}

// RotateKey gives a key a new secret. The current one becomes the
// previous secret, which works until graceUntil; any older one stops.
func (s *Store) RotateKey(ctx context.Context, keyID, prefix, hash string, graceUntil time.Time) error {
	t := now()
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE api_keys SET previous_hash = secret_hash, previous_expires_at = ?, secret_hash = ?, prefix = ?, updated_at = ? WHERE id = ? AND revoked_at IS NULL`),
		graceUntil.UnixMilli(), hash, prefix, t, keyID)
	return affected(res, err)
}

// RevokeKey marks a key revoked. Revoking twice is not an error.
func (s *Store) RevokeKey(ctx context.Context, keyID string) error {
	t := now()
	res, err := s.db.ExecContext(ctx, s.q(`UPDATE api_keys SET revoked_at = COALESCE(revoked_at, ?), updated_at = ? WHERE id = ?`), t, t, keyID)
	return affected(res, err)
}

// --- snapshot ---

// Snapshot is everything the request path needs, loaded in one go.
type Snapshot struct {
	Orgs  map[string]*Org
	Teams map[string]*Team
	// Keys holds keys that are not revoked. Expiry is checked per request.
	Keys []*Key
}

// LoadSnapshot reads all organizations, teams and unrevoked keys.
func (s *Store) LoadSnapshot(ctx context.Context) (*Snapshot, error) {
	snap := &Snapshot{Orgs: map[string]*Org{}, Teams: map[string]*Team{}}
	rows, err := s.db.QueryContext(ctx, `SELECT `+orgCols+` FROM orgs`)
	if err != nil {
		return nil, err
	}
	orgs, _, err := collect(rows, -1, scanOrg)
	if err != nil {
		return nil, err
	}
	for _, o := range orgs {
		snap.Orgs[o.ID] = o
	}
	if rows, err = s.db.QueryContext(ctx, `SELECT `+teamCols+` FROM teams`); err != nil {
		return nil, err
	}
	teams, _, err := collect(rows, -1, scanTeam)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		snap.Teams[t.ID] = t
	}
	if rows, err = s.db.QueryContext(ctx, `SELECT `+keyCols+` FROM api_keys WHERE revoked_at IS NULL`); err != nil {
		return nil, err
	}
	snap.Keys, _, err = collect(rows, -1, scanKey)
	return snap, err
}

// --- usage ---

// InsertUsage writes a batch of usage records and updates last_used_at
// on the keys involved, in one transaction.
func (s *Store) InsertUsage(ctx context.Context, recs []usage.Record) error {
	if len(recs) == 0 {
		return nil
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, s.q(`INSERT INTO usage (request_id, ts, org_id, team_id, key_id, endpoint, model, provider, upstream_model, status, latency_ms, input_tokens, output_tokens, cached_input_tokens, reasoning_tokens, images, characters, audio_seconds, video_seconds, cost_usd, cache_hit, saved_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`))
		if err != nil {
			return err
		}
		defer stmt.Close()
		lastUsed := map[string]int64{}
		for _, r := range recs {
			ts := r.Time.UnixMilli()
			hit := 0
			if r.CacheHit {
				hit = 1
			}
			if _, err := stmt.ExecContext(ctx, r.RequestID, ts, r.OrgID, r.TeamID, r.KeyID, r.Endpoint, r.Model, r.Provider, r.UpstreamModel,
				r.Status, r.LatencyMS, r.Tokens.InputTokens, r.Tokens.OutputTokens, r.Tokens.CachedInputTokens, r.Tokens.ReasoningTokens,
				r.Tokens.Images, r.Tokens.Characters, r.Tokens.AudioSeconds, r.Tokens.VideoSeconds, r.CostUSD, hit, r.SavedUSD); err != nil {
				return err
			}
			lastUsed[r.KeyID] = max(lastUsed[r.KeyID], ts)
		}
		for keyID, ts := range lastUsed {
			if _, err := tx.ExecContext(ctx, s.q(`UPDATE api_keys SET last_used_at = ? WHERE id = ? AND (last_used_at IS NULL OR last_used_at < ?)`), ts, keyID, ts); err != nil {
				return err
			}
		}
		return nil
	})
}

// UsageQuery filters and groups usage. Zero values mean no filter.
type UsageQuery struct {
	OrgID, TeamID, KeyID string
	From, To             time.Time
	// GroupBy is "model", "team", "key", "day", "provider" or "endpoint".
	GroupBy string
}

// UsageRow is one aggregated group.
type UsageRow struct {
	Group             string  `json:"group"`
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	CachedInputTokens int64   `json:"cached_input_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	Images            int64   `json:"images"`
	Characters        int64   `json:"characters"`
	AudioSeconds      float64 `json:"audio_seconds"`
	VideoSeconds      float64 `json:"video_seconds"`
	CostUSD           float64 `json:"cost_usd"`
	// CacheHits are requests answered from the response cache, and
	// SavedUSD what those answers cost when they were first made.
	CacheHits int64   `json:"cache_hits"`
	SavedUSD  float64 `json:"saved_usd"`
}

// QueryUsage aggregates usage between From (inclusive) and To (exclusive).
func (s *Store) QueryUsage(ctx context.Context, uq UsageQuery) ([]UsageRow, error) {
	group := map[string]string{
		"model":    "model",
		"team":     "team_id",
		"key":      "key_id",
		"day":      "CAST(ts / 86400000 AS TEXT)",
		"provider": "provider",
		"endpoint": "endpoint",
	}[uq.GroupBy]
	if group == "" {
		return nil, fmt.Errorf("group_by must be model, team, key, day, provider or endpoint")
	}
	where := []string{"ts >= ?", "ts < ?"}
	args := []any{uq.From.UnixMilli(), uq.To.UnixMilli()}
	for col, v := range map[string]string{"org_id": uq.OrgID, "team_id": uq.TeamID, "key_id": uq.KeyID} {
		if v != "" {
			where = append(where, col+" = ?")
			args = append(args, v)
		}
	}
	query := `SELECT ` + group + ` AS g, COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cached_input_tokens), 0), COALESCE(SUM(reasoning_tokens), 0), COALESCE(SUM(images), 0), COALESCE(SUM(characters), 0),
		COALESCE(SUM(audio_seconds), 0), COALESCE(SUM(video_seconds), 0), COALESCE(SUM(cost_usd), 0),
		COALESCE(SUM(cache_hit), 0), COALESCE(SUM(saved_usd), 0)
		FROM usage WHERE ` + strings.Join(where, " AND ") + ` GROUP BY g ORDER BY g`
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Group, &r.Requests, &r.InputTokens, &r.OutputTokens, &r.CachedInputTokens, &r.ReasoningTokens,
			&r.Images, &r.Characters, &r.AudioSeconds, &r.VideoSeconds, &r.CostUSD, &r.CacheHits, &r.SavedUSD); err != nil {
			return nil, err
		}
		if uq.GroupBy == "day" {
			if days, err := strconv.ParseInt(r.Group, 10, 64); err == nil {
				r.Group = toTime(days * 86400000).Format("2006-01-02")
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TargetHealth is how one upstream target answered since a time, from
// the final attempt of each request.
type TargetHealth struct {
	Provider      string     `json:"-"`
	UpstreamModel string     `json:"-"`
	Requests      int64      `json:"requests"`
	Errors        int64      `json:"errors"`
	ErrorRate     float64    `json:"error_rate"`
	AvgLatencyMS  int64      `json:"avg_latency_ms"`
	LastErrorAt   *time.Time `json:"last_error_at,omitempty"`
}

// Health aggregates usage since a time by provider and upstream model.
// An error is a status of 400 or above.
func (s *Store) Health(ctx context.Context, since time.Time) ([]TargetHealth, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT provider, upstream_model, COUNT(*),
		COALESCE(SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END), 0), COALESCE(AVG(latency_ms), 0),
		MAX(CASE WHEN status >= 400 THEN ts END)
		FROM usage WHERE ts >= ? GROUP BY provider, upstream_model`), since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TargetHealth{}
	for rows.Next() {
		var h TargetHealth
		var avg float64
		var lastErr sql.NullInt64
		if err := rows.Scan(&h.Provider, &h.UpstreamModel, &h.Requests, &h.Errors, &avg, &lastErr); err != nil {
			return nil, err
		}
		h.AvgLatencyMS = int64(avg)
		if h.Requests > 0 {
			h.ErrorRate = float64(h.Errors) / float64(h.Requests)
		}
		h.LastErrorAt = toTimePtr(lastErr)
		out = append(out, h)
	}
	return out, rows.Err()
}

// SpendSince returns total cost per scope id since a time, for seeding
// budget counters. Keys of the result are org, team and key ids.
func (s *Store) SpendSince(ctx context.Context, since time.Time) (map[string]float64, error) {
	out := map[string]float64{}
	for _, col := range []string{"org_id", "team_id", "key_id"} {
		rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+col+`, COALESCE(SUM(cost_usd), 0) FROM usage WHERE ts >= ? GROUP BY `+col), since.UnixMilli())
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var scope string
			var cost float64
			if err := rows.Scan(&scope, &cost); err != nil {
				rows.Close()
				return nil, err
			}
			out[scope] += cost
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PruneUsage deletes usage rows older than a time.
func (s *Store) PruneUsage(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM usage WHERE ts < ?`), before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- audit ---

// AuditEntry records one administrative change.
type AuditEntry struct {
	ID           string          `json:"id"`
	Time         time.Time       `json:"ts"`
	Actor        string          `json:"actor"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	OrgID        string          `json:"org_id"`
	Details      json.RawMessage `json:"details"`
}

// AppendAudit writes an audit entry.
func (s *Store) AppendAudit(ctx context.Context, e *AuditEntry) error {
	e.ID = id.New("audit")
	e.Time = toTime(now())
	if e.Details == nil {
		e.Details = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO audit_log (id, ts, actor, action, resource_type, resource_id, org_id, details) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		e.ID, e.Time.UnixMilli(), e.Actor, e.Action, e.ResourceType, e.ResourceID, e.OrgID, string(e.Details))
	return err
}

// ListAudit returns audit entries newest first; After is an entry id.
func (s *Store) ListAudit(ctx context.Context, orgID string, p Page) ([]*AuditEntry, bool, error) {
	n := limit(p)
	query := `SELECT id, ts, actor, action, resource_type, resource_id, org_id, details FROM audit_log WHERE (? = '' OR org_id = ?)`
	args := []any{orgID, orgID}
	if p.After != "" {
		query += ` AND id < ?`
		args = append(args, p.After)
	}
	rows, err := s.db.QueryContext(ctx, s.q(query+` ORDER BY id DESC LIMIT ?`), append(args, n+1)...)
	if err != nil {
		return nil, false, err
	}
	return collect(rows, n, func(row interface{ Scan(...any) error }) (*AuditEntry, error) {
		var e AuditEntry
		var ts int64
		var details string
		if err := row.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.ResourceType, &e.ResourceID, &e.OrgID, &details); err != nil {
			return nil, err
		}
		e.Time, e.Details = toTime(ts), json.RawMessage(details)
		return &e, nil
	})
}

// collect scans rows; with n >= 0 it returns at most n and reports
// whether more were available.
func collect[T any](rows *sql.Rows, n int, scan func(interface{ Scan(...any) error }) (T, error)) ([]T, bool, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if n >= 0 && len(out) > n {
		return out[:n], true, nil
	}
	return out, false, nil
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
