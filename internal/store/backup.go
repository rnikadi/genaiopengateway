package store

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// BackupFormat names the archive format written by Backup. It is the first
// field of the archive's header line.
const BackupFormat = "genaiopengateway-backup/1"

// BackupHeader is the first line of a backup archive.
type BackupHeader struct {
	Format string `json:"format"`
	// Schema is the last migration applied to the database the backup
	// was taken from. Restore refuses an archive newer than its binary.
	Schema    string           `json:"schema"`
	Driver    string           `json:"driver"`
	CreatedAt time.Time        `json:"created_at"`
	Tables    map[string]int64 `json:"tables"`
	// KeyCheck identifies the encryption key the stored secrets need,
	// without revealing it (see KeyCheck).
	KeyCheck string `json:"key_check,omitempty"`
}

// BackupOptions adds to a backup's header.
type BackupOptions struct {
	// KeyCheck is KeyCheck of the gateway's secrets.encryption_key.
	KeyCheck string
}

// KeyCheck returns a short fingerprint of an encryption key, so that a
// restore can tell the target holds the key the archive's secrets need.
// It is a hash under a fixed label; the key cannot be recovered from it.
func KeyCheck(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("genaiopengateway backup key check\x00" + key))
	return hex.EncodeToString(sum[:6])
}

// backupRow is one line of the archive after the header.
type backupRow struct {
	Table string         `json:"t"`
	Row   map[string]any `json:"r"`
}

// notBackedUp lists tables a backup leaves out: the migration record,
// which the target keeps for itself; the config revision counter, which
// Restore moves on instead; and admin sign-in sessions, so that a restored
// gateway does not accept the source's cookies.
var notBackedUp = map[string]bool{"schema_migrations": true, "config_revision": true, "admin_sessions": true}

var createTable = regexp.MustCompile(`(?i)CREATE TABLE\s+(?:IF NOT EXISTS\s+)?([a-z0-9_]+)`)

// backupTables returns the tables a backup holds, in the order the
// migrations create them, so that a table comes after the tables its
// foreign keys name.
func backupTables() ([]string, error) {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		body, _ := migrations.ReadFile(f)
		for _, m := range createTable.FindAllStringSubmatch(stripComments(string(body)), -1) {
			name := strings.ToLower(m[1])
			if !notBackedUp[name] && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out, nil
}

// latestMigration is the newest migration this binary knows.
func latestMigration() string {
	files, _ := fs.Glob(migrations, "migrations/*.sql")
	sort.Strings(files)
	if len(files) == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(files[len(files)-1], "migrations/"), ".sql")
}

// SchemaVersion returns the last migration applied to the database.
func (s *Store) SchemaVersion(ctx context.Context) (string, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return v.String, err
}

// Backup writes every table's rows to w as a gzip-compressed archive of
// JSON lines: a BackupHeader, then one line per row. The rows are read in
// one transaction, so the archive is a consistent snapshot taken while the
// gateway keeps serving. Stored secrets stay encrypted with the gateway's
// secrets.encryption_key; the archive is useless without that key, and the
// key is never in it.
func (s *Store) Backup(ctx context.Context, w io.Writer, o BackupOptions) (BackupHeader, error) {
	tables, err := backupTables()
	if err != nil {
		return BackupHeader{}, err
	}
	opts := &sql.TxOptions{ReadOnly: true}
	if s.postgres {
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := s.db.BeginTx(ctx, opts)
	if err != nil {
		return BackupHeader{}, err
	}
	defer tx.Rollback()

	h := BackupHeader{Format: BackupFormat, Driver: s.driver(), CreatedAt: time.Now().UTC().Truncate(time.Second), Tables: map[string]int64{}, KeyCheck: o.KeyCheck}
	var schema sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&schema); err != nil {
		return h, err
	}
	h.Schema = schema.String
	for _, t := range tables {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&n); err != nil {
			return h, fmt.Errorf("count %s: %w", t, err)
		}
		h.Tables[t] = n
	}

	gz := gzip.NewWriter(w)
	enc := json.NewEncoder(gz)
	if err := enc.Encode(h); err != nil {
		return h, err
	}
	for _, t := range tables {
		if err := dumpTable(ctx, tx, t, enc); err != nil {
			return h, fmt.Errorf("back up %s: %w", t, err)
		}
	}
	return h, gz.Close()
}

func dumpTable(ctx context.Context, tx *sql.Tx, table string, enc *json.Encoder) error {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+table)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		r := make(map[string]any, len(cols))
		for i, c := range cols {
			switch v := vals[i].(type) {
			case []byte:
				r[c] = string(v)
			case time.Time:
				// Every timestamp is stored as Unix milliseconds; a driver
				// that hands back a time still round-trips.
				r[c] = v.UnixMilli()
			default:
				r[c] = v
			}
		}
		if err := enc.Encode(backupRow{Table: table, Row: r}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// RestoreOptions changes how Restore treats a database that has data.
type RestoreOptions struct {
	// Replace deletes what the database holds before restoring. Without
	// it, Restore refuses a database that is not empty.
	Replace bool
}

// ErrNotEmpty is returned by Restore when the target database has data
// and RestoreOptions.Replace is not set.
var ErrNotEmpty = errors.New("the database is not empty")

// ReadBackupHeader reads the header of an archive without restoring it.
func ReadBackupHeader(r io.Reader) (BackupHeader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return BackupHeader{}, fmt.Errorf("not a gateway backup: %w", err)
	}
	defer gz.Close()
	dec := json.NewDecoder(gz)
	var h BackupHeader
	if err := dec.Decode(&h); err != nil || h.Format != BackupFormat {
		return h, fmt.Errorf("not a gateway backup (format %q)", h.Format)
	}
	return h, nil
}

// Restore loads an archive written by Backup, from either driver, into
// this database in one transaction: on any error nothing changes. The
// archive's schema must not be newer than this binary's migrations; an
// older archive is fine, since migrations only add columns with defaults.
// Every replica reloads the restored configuration within the refresh
// interval.
func (s *Store) Restore(ctx context.Context, r io.Reader, opts RestoreOptions) (BackupHeader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return BackupHeader{}, fmt.Errorf("not a gateway backup: %w", err)
	}
	defer gz.Close()
	dec := json.NewDecoder(bufio.NewReaderSize(gz, 1<<20))
	dec.UseNumber()
	var h BackupHeader
	if err := dec.Decode(&h); err != nil || h.Format != BackupFormat {
		return h, fmt.Errorf("not a gateway backup (format %q)", h.Format)
	}
	if latest := latestMigration(); h.Schema > latest {
		return h, fmt.Errorf("the backup is from a newer gateway (schema %s, this binary knows up to %s); restore it with that version or later", h.Schema, latest)
	}
	tables, err := backupTables()
	if err != nil {
		return h, err
	}
	known := map[string]bool{}
	for _, t := range tables {
		known[t] = true
	}
	for t := range h.Tables {
		if !known[t] {
			return h, fmt.Errorf("the backup has a table this gateway does not know: %s", t)
		}
	}

	err = s.tx(ctx, func(tx *sql.Tx) error {
		var nonEmpty []string
		for _, t := range tables {
			var n int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&n); err != nil {
				return fmt.Errorf("count %s: %w", t, err)
			}
			if n > 0 {
				nonEmpty = append(nonEmpty, t)
			}
		}
		if len(nonEmpty) > 0 {
			if !opts.Replace {
				return fmt.Errorf("%w (%s); restore into a new database, or pass -replace to delete its data first", ErrNotEmpty, strings.Join(nonEmpty, ", "))
			}
			for _, t := range slices.Backward(tables) {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
					return fmt.Errorf("clear %s: %w", t, err)
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions`); err != nil {
				return fmt.Errorf("clear admin_sessions: %w", err)
			}
		}

		cols := map[string][]string{}
		stmts := map[string]*sql.Stmt{}
		defer func() {
			for _, st := range stmts {
				st.Close()
			}
		}()
		got := map[string]int64{}
		order := map[string]int{}
		for i, t := range tables {
			order[t] = i
		}
		last := -1
		for {
			var row backupRow
			if err := dec.Decode(&row); err == io.EOF {
				break
			} else if err != nil {
				return fmt.Errorf("read the backup: %w", err)
			}
			if !known[row.Table] {
				return fmt.Errorf("the backup has a table this gateway does not know: %s", row.Table)
			}
			if order[row.Table] < last {
				return fmt.Errorf("the backup's rows are out of order at %s", row.Table)
			}
			last = order[row.Table]
			c, ok := cols[row.Table]
			if !ok {
				for k := range row.Row {
					c = append(c, k)
				}
				sort.Strings(c)
				for _, k := range c {
					if !validColumn(k) {
						return fmt.Errorf("the backup has an invalid column name in %s", row.Table)
					}
				}
				marks := strings.TrimSuffix(strings.Repeat("?, ", len(c)), ", ")
				st, err := tx.PrepareContext(ctx, s.q(`INSERT INTO `+row.Table+` (`+strings.Join(c, ", ")+`) VALUES (`+marks+`)`))
				if err != nil {
					return fmt.Errorf("restore %s: %w", row.Table, err)
				}
				cols[row.Table] = c
				stmts[row.Table] = st
			}
			if len(row.Row) != len(c) {
				return fmt.Errorf("the backup's rows of %s do not all have the same columns", row.Table)
			}
			args := make([]any, len(c))
			for i, k := range c {
				v, ok := row.Row[k]
				if !ok {
					return fmt.Errorf("the backup's rows of %s do not all have the same columns", row.Table)
				}
				args[i] = restoreValue(v)
			}
			if _, err := stmts[row.Table].ExecContext(ctx, args...); err != nil {
				return fmt.Errorf("restore %s: %w", row.Table, err)
			}
			got[row.Table]++
		}
		for t, n := range h.Tables {
			if got[t] != n {
				return fmt.Errorf("the backup is incomplete: %s has %d rows of %d", t, got[t], n)
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE config_revision SET rev = rev + 1 WHERE id = 1`)
		return err
	})
	return h, err
}

var columnName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func validColumn(c string) bool { return columnName.MatchString(c) }

// restoreValue turns a JSON value back into what the column stores:
// numbers without a fraction are integers, others floats.
func restoreValue(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, _ := n.Float64()
	return f
}

func (s *Store) driver() string {
	if s.postgres {
		return "postgres"
	}
	return "sqlite"
}
