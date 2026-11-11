package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/id"
)

// SCIMUser is a user the identity provider provisioned (docs/scim.md).
// UserName is unique without case.
type SCIMUser struct {
	ID          string    `json:"id"`
	UserName    string    `json:"user_name"`
	ExternalID  string    `json:"external_id,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Email       string    `json:"email,omitempty"`
	Active      bool      `json:"active"`
	Groups      []SCIMRef `json:"groups"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SCIMGroup is a provisioned group. DisplayName is unique without case,
// and is what role mappings name in group.
type SCIMGroup struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	ExternalID  string    `json:"external_id,omitempty"`
	Members     []SCIMRef `json:"members"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SCIMRef names a group of a user or a member of a group.
type SCIMRef struct {
	ID      string `json:"id"`
	Display string `json:"display"`
}

// SCIMFilter selects users or groups by one attribute; an empty Attr
// matches all. Attr is user_name, external_id, email or display_name.
type SCIMFilter struct {
	Attr, Value string
}

func (f SCIMFilter) where(table string) (string, []any, error) {
	switch f.Attr {
	case "":
		return "", nil, nil
	case "user_name":
		return " WHERE user_key = ?", []any{strings.ToLower(f.Value)}, nil
	case "display_name":
		if table == "scim_groups" {
			return " WHERE group_key = ?", []any{strings.ToLower(f.Value)}, nil
		}
		return " WHERE display_name = ?", []any{f.Value}, nil
	case "external_id":
		return " WHERE external_id = ?", []any{f.Value}, nil
	case "email":
		return " WHERE email = ?", []any{strings.ToLower(f.Value)}, nil
	}
	return "", nil, fmt.Errorf("unsupported filter attribute %s", f.Attr)
}

const scimUserCols = `id, user_name, external_id, display_name, email, active, created_at, updated_at`

func scanSCIMUser(row interface{ Scan(...any) error }) (*SCIMUser, error) {
	u := &SCIMUser{Groups: []SCIMRef{}}
	var active int
	var created, updated int64
	if err := row.Scan(&u.ID, &u.UserName, &u.ExternalID, &u.DisplayName, &u.Email, &active, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Active, u.CreatedAt, u.UpdatedAt = active == 1, toTime(created), toTime(updated)
	return u, nil
}

// isUnique reports a unique constraint violation, on SQLite or Postgres.
func isUnique(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "duplicate key"))
}

// CreateSCIMUser inserts a user, assigning its id and times. A taken
// user name is ErrConflict.
func (s *Store) CreateSCIMUser(ctx context.Context, u *SCIMUser) error {
	u.ID = id.New("usr")
	u.CreatedAt = toTime(now())
	u.UpdatedAt = u.CreatedAt
	u.Email = strings.ToLower(u.Email)
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO scim_users (id, user_name, user_key, external_id, display_name, email, active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		u.ID, u.UserName, strings.ToLower(u.UserName), u.ExternalID, u.DisplayName, u.Email, boolInt(u.Active), u.CreatedAt.UnixMilli(), u.UpdatedAt.UnixMilli())
	if isUnique(err) {
		return fmt.Errorf("%w: a user named %q exists", ErrConflict, u.UserName)
	}
	if u.Groups == nil {
		u.Groups = []SCIMRef{}
	}
	return err
}

// GetSCIMUser returns a user with its groups.
func (s *Store) GetSCIMUser(ctx context.Context, userID string) (*SCIMUser, error) {
	u, err := scanSCIMUser(s.db.QueryRowContext(ctx, s.q(`SELECT `+scimUserCols+` FROM scim_users WHERE id = ?`), userID))
	if err != nil {
		return nil, err
	}
	u.Groups, err = s.refs(ctx, `SELECT g.id, g.display_name FROM scim_members m JOIN scim_groups g ON g.id = m.group_id WHERE m.user_id = ? ORDER BY g.display_name`, u.ID)
	return u, err
}

// FindSCIMUser returns the user whose user name or email is the given
// address, ignoring case, with its groups.
func (s *Store) FindSCIMUser(ctx context.Context, email string) (*SCIMUser, error) {
	key := strings.ToLower(email)
	u, err := scanSCIMUser(s.db.QueryRowContext(ctx, s.q(`SELECT `+scimUserCols+` FROM scim_users WHERE user_key = ? OR email = ? ORDER BY user_key = ? DESC LIMIT 1`), key, key, key))
	if err != nil {
		return nil, err
	}
	return s.GetSCIMUser(ctx, u.ID)
}

// ListSCIMUsers returns users from a 0-based offset, with the total that
// match the filter.
func (s *Store) ListSCIMUsers(ctx context.Context, f SCIMFilter, offset, count int) ([]*SCIMUser, int, error) {
	where, args, err := f.where("scim_users")
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM scim_users`+where), args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+scimUserCols+` FROM scim_users`+where+` ORDER BY id LIMIT ? OFFSET ?`), append(args, count, offset)...)
	if err != nil {
		return nil, 0, err
	}
	var users []*SCIMUser
	for rows.Next() {
		u, err := scanSCIMUser(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		users = append(users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	for _, u := range users {
		if u.Groups, err = s.refs(ctx, `SELECT g.id, g.display_name FROM scim_members m JOIN scim_groups g ON g.id = m.group_id WHERE m.user_id = ? ORDER BY g.display_name`, u.ID); err != nil {
			return nil, 0, err
		}
	}
	return users, total, nil
}

// UpdateSCIMUser replaces a user's attributes.
func (s *Store) UpdateSCIMUser(ctx context.Context, u *SCIMUser) error {
	u.UpdatedAt = toTime(now())
	u.Email = strings.ToLower(u.Email)
	err := affected(s.db.ExecContext(ctx, s.q(`UPDATE scim_users SET user_name = ?, user_key = ?, external_id = ?, display_name = ?, email = ?, active = ?, updated_at = ? WHERE id = ?`),
		u.UserName, strings.ToLower(u.UserName), u.ExternalID, u.DisplayName, u.Email, boolInt(u.Active), u.UpdatedAt.UnixMilli(), u.ID))
	if isUnique(err) {
		return fmt.Errorf("%w: a user named %q exists", ErrConflict, u.UserName)
	}
	return err
}

// DeleteSCIMUser removes a user and its memberships.
func (s *Store) DeleteSCIMUser(ctx context.Context, userID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM scim_members WHERE user_id = ?`), userID); err != nil {
			return err
		}
		return affected(tx.ExecContext(ctx, s.q(`DELETE FROM scim_users WHERE id = ?`), userID))
	})
}

func (s *Store) refs(ctx context.Context, query, arg string) ([]SCIMRef, error) {
	rows, err := s.db.QueryContext(ctx, s.q(query), arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SCIMRef{}
	for rows.Next() {
		var r SCIMRef
		if err := rows.Scan(&r.ID, &r.Display); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const scimGroupCols = `id, display_name, external_id, created_at, updated_at`

func scanSCIMGroup(row interface{ Scan(...any) error }) (*SCIMGroup, error) {
	g := &SCIMGroup{Members: []SCIMRef{}}
	var created, updated int64
	if err := row.Scan(&g.ID, &g.DisplayName, &g.ExternalID, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	g.CreatedAt, g.UpdatedAt = toTime(created), toTime(updated)
	return g, nil
}

// CreateSCIMGroup inserts a group with its members. A taken name is
// ErrConflict, and an unknown member ErrNotFound.
func (s *Store) CreateSCIMGroup(ctx context.Context, g *SCIMGroup, memberIDs []string) error {
	g.ID = id.New("grp")
	g.CreatedAt = toTime(now())
	g.UpdatedAt = g.CreatedAt
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO scim_groups (id, display_name, group_key, external_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`),
			g.ID, g.DisplayName, strings.ToLower(g.DisplayName), g.ExternalID, g.CreatedAt.UnixMilli(), g.UpdatedAt.UnixMilli()); err != nil {
			return err
		}
		return s.addMembers(ctx, tx, g.ID, memberIDs)
	})
	if isUnique(err) {
		return fmt.Errorf("%w: a group named %q exists", ErrConflict, g.DisplayName)
	}
	return err
}

func (s *Store) addMembers(ctx context.Context, tx *sql.Tx, groupID string, userIDs []string) error {
	for _, uid := range userIDs {
		var n int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM scim_users WHERE id = ?`), uid).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: no user %s", ErrNotFound, uid)
		}
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM scim_members WHERE group_id = ? AND user_id = ?`), groupID, uid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO scim_members (group_id, user_id) VALUES (?, ?)`), groupID, uid); err != nil {
			return err
		}
	}
	return nil
}

// GetSCIMGroup returns a group with its members.
func (s *Store) GetSCIMGroup(ctx context.Context, groupID string) (*SCIMGroup, error) {
	g, err := scanSCIMGroup(s.db.QueryRowContext(ctx, s.q(`SELECT `+scimGroupCols+` FROM scim_groups WHERE id = ?`), groupID))
	if err != nil {
		return nil, err
	}
	g.Members, err = s.refs(ctx, `SELECT u.id, u.user_name FROM scim_members m JOIN scim_users u ON u.id = m.user_id WHERE m.group_id = ? ORDER BY u.user_key`, g.ID)
	return g, err
}

// ListSCIMGroups returns groups from a 0-based offset, with the total
// that match. Members are left out unless withMembers is set.
func (s *Store) ListSCIMGroups(ctx context.Context, f SCIMFilter, offset, count int, withMembers bool) ([]*SCIMGroup, int, error) {
	where, args, err := f.where("scim_groups")
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM scim_groups`+where), args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT `+scimGroupCols+` FROM scim_groups`+where+` ORDER BY id LIMIT ? OFFSET ?`), append(args, count, offset)...)
	if err != nil {
		return nil, 0, err
	}
	var groups []*SCIMGroup
	for rows.Next() {
		g, err := scanSCIMGroup(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		groups = append(groups, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if withMembers {
		for _, g := range groups {
			if g.Members, err = s.refs(ctx, `SELECT u.id, u.user_name FROM scim_members m JOIN scim_users u ON u.id = m.user_id WHERE m.group_id = ? ORDER BY u.user_key`, g.ID); err != nil {
				return nil, 0, err
			}
		}
	}
	return groups, total, nil
}

// UpdateSCIMGroup renames a group. With members non-nil, it also
// replaces the group's members, then adds and removes the ones given.
func (s *Store) UpdateSCIMGroup(ctx context.Context, g *SCIMGroup, members, add, remove []string) error {
	g.UpdatedAt = toTime(now())
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := affected(tx.ExecContext(ctx, s.q(`UPDATE scim_groups SET display_name = ?, group_key = ?, external_id = ?, updated_at = ? WHERE id = ?`),
			g.DisplayName, strings.ToLower(g.DisplayName), g.ExternalID, g.UpdatedAt.UnixMilli(), g.ID)); err != nil {
			return err
		}
		if members != nil {
			if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM scim_members WHERE group_id = ?`), g.ID); err != nil {
				return err
			}
			if err := s.addMembers(ctx, tx, g.ID, members); err != nil {
				return err
			}
		}
		if err := s.addMembers(ctx, tx, g.ID, add); err != nil {
			return err
		}
		for _, uid := range remove {
			if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM scim_members WHERE group_id = ? AND user_id = ?`), g.ID, uid); err != nil {
				return err
			}
		}
		return nil
	})
	if isUnique(err) {
		return fmt.Errorf("%w: a group named %q exists", ErrConflict, g.DisplayName)
	}
	return err
}

// DeleteSCIMGroup removes a group and its memberships.
func (s *Store) DeleteSCIMGroup(ctx context.Context, groupID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q(`DELETE FROM scim_members WHERE group_id = ?`), groupID); err != nil {
			return err
		}
		return affected(tx.ExecContext(ctx, s.q(`DELETE FROM scim_groups WHERE id = ?`), groupID))
	})
}

// DeleteSessionsByEmail signs out every session of a user, by the email
// the identity provider gave at sign-in, ignoring case.
func (s *Store) DeleteSessionsByEmail(ctx context.Context, emails ...string) (int64, error) {
	var total int64
	for _, e := range emails {
		if e == "" {
			continue
		}
		res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM admin_sessions WHERE lower(email) = lower(?)`), e)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
