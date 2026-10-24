package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ConfigObject is a configuration object created through the admin API
// (ADR 0023). Spec holds no secrets; Secrets holds them sealed, as the
// server encrypted them.
type ConfigObject struct {
	Kind      string
	Name      string
	Spec      string
	Secrets   string
	UpdatedAt time.Time
	UpdatedBy string
}

// ConfigRevision returns a number that changes with every write to
// configuration objects.
func (s *Store) ConfigRevision(ctx context.Context) (int64, error) {
	var rev int64
	err := s.db.QueryRowContext(ctx, `SELECT rev FROM config_revision WHERE id = 1`).Scan(&rev)
	return rev, err
}

// ListConfigObjects returns every stored object and the revision they
// were read at.
func (s *Store) ListConfigObjects(ctx context.Context) ([]*ConfigObject, int64, error) {
	var out []*ConfigObject
	var rev int64
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT rev FROM config_revision WHERE id = 1`).Scan(&rev); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT kind, name, spec, secrets, updated_at, updated_by FROM config_objects ORDER BY kind, name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o := &ConfigObject{}
			var at int64
			if err := rows.Scan(&o.Kind, &o.Name, &o.Spec, &o.Secrets, &at, &o.UpdatedBy); err != nil {
				return err
			}
			o.UpdatedAt = toTime(at)
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, rev, err
}

// GetConfigObject returns one stored object.
func (s *Store) GetConfigObject(ctx context.Context, kind, name string) (*ConfigObject, error) {
	o := &ConfigObject{Kind: kind, Name: name}
	var at int64
	err := s.db.QueryRowContext(ctx, s.q(`SELECT spec, secrets, updated_at, updated_by FROM config_objects WHERE kind = ? AND name = ?`), kind, name).
		Scan(&o.Spec, &o.Secrets, &at, &o.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	o.UpdatedAt = toTime(at)
	return o, err
}

// PutConfigObject creates or replaces an object and reports whether it
// was created.
func (s *Store) PutConfigObject(ctx context.Context, o *ConfigObject) (bool, error) {
	o.UpdatedAt = toTime(now())
	var created bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(`UPDATE config_objects SET spec = ?, secrets = ?, updated_at = ?, updated_by = ? WHERE kind = ? AND name = ?`),
			o.Spec, o.Secrets, o.UpdatedAt.UnixMilli(), o.UpdatedBy, o.Kind, o.Name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			created = true
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO config_objects (kind, name, spec, secrets, updated_at, updated_by) VALUES (?, ?, ?, ?, ?, ?)`),
				o.Kind, o.Name, o.Spec, o.Secrets, o.UpdatedAt.UnixMilli(), o.UpdatedBy); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE config_revision SET rev = rev + 1 WHERE id = 1`)
		return err
	})
	return created, err
}

// DeleteConfigObject deletes an object.
func (s *Store) DeleteConfigObject(ctx context.Context, kind, name string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM config_objects WHERE kind = ? AND name = ?`), kind, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `UPDATE config_revision SET rev = rev + 1 WHERE id = 1`)
		return err
	})
}
