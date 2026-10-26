package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/router"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Teams' own provider credentials (docs/team-credentials.md).

// teamCredentials holds every team's provider API keys and the adapters
// built from them for one router.
type teamCredentials struct {
	keys     map[string]map[string]string // team, provider, API key
	router   *router.Router
	adapters map[string]router.Credentials
}

func credentialAD(teamID, provider string) []byte {
	return []byte("team-credential/" + teamID + "/" + provider)
}

// loadTeamCredentials reads and decrypts every team's credentials. A
// credential that cannot be opened is logged and its provider fails for
// that team rather than falling back to the shared credential.
func (s *Server) loadTeamCredentials(ctx context.Context) error {
	rows, err := s.store.ListTeamCredentials(ctx, "")
	if err != nil {
		return err
	}
	keys := map[string]map[string]string{}
	for _, c := range rows {
		key, err := s.openCredential(c)
		if err != nil {
			s.log.Error("a team's provider credential cannot be opened; its requests to that provider fail", "team_id", c.TeamID, "provider", c.Provider, "err", err)
		}
		if keys[c.TeamID] == nil {
			keys[c.TeamID] = map[string]string{}
		}
		keys[c.TeamID][c.Provider] = key
	}
	if prev := s.teamCreds.Load(); prev != nil && maps.EqualFunc(prev.keys, keys, maps.Equal) {
		return nil
	}
	s.teamCreds.Store(&teamCredentials{keys: keys})
	return nil
}

func (s *Server) openCredential(c *store.TeamCredential) (string, error) {
	s.cfgMu.Lock()
	sc := s.file.Secrets
	s.cfgMu.Unlock()
	box, err := secretBox(sc)
	if err != nil {
		return "", err
	}
	plain, err := box.Open(c.Secret, credentialAD(c.TeamID, c.Provider))
	return string(plain), err
}

// teamContext returns ctx with the adapters of a team's own provider
// credentials, which requests use in place of the shared ones.
func (s *Server) teamContext(ctx context.Context, teamID string) context.Context {
	tc := s.teamCreds.Load()
	if tc == nil || len(tc.keys[teamID]) == 0 {
		return ctx
	}
	rt := s.rt.Load().router
	if tc.router != rt {
		s.credsMu.Lock()
		if tc = s.teamCreds.Load(); tc.router != rt {
			next := &teamCredentials{keys: tc.keys, router: rt, adapters: map[string]router.Credentials{}}
			for team, byProvider := range tc.keys {
				c := router.Credentials{}
				for name, key := range byProvider {
					if key == "" {
						c[name] = router.Unavailable(fmt.Errorf("the team's credential for %s cannot be decrypted", name))
						continue
					}
					c[name], _ = rt.WithAPIKey(name, key)
				}
				next.adapters[team] = c
			}
			s.teamCreds.Store(next)
			tc = next
		}
		s.credsMu.Unlock()
	}
	return router.WithCredentials(ctx, tc.adapters[teamID])
}

// --- admin API ---

type credentialEntry struct {
	Provider  string     `json:"provider"`
	Type      string     `json:"type"`
	Set       bool       `json:"set"`
	Supported bool       `json:"supported"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
}

// listTeamCredentials lists every provider with whether the team has its
// own credential for it.
func (s *Server) listTeamCredentials(w http.ResponseWriter, r *http.Request) error {
	t, err := s.store.GetTeam(r.Context(), r.PathValue("team_id"))
	if err != nil {
		return err
	}
	if err := read(r, t.OrgID); err != nil {
		return err
	}
	rows, err := s.store.ListTeamCredentials(r.Context(), t.ID)
	if err != nil {
		return err
	}
	rt := s.rt.Load()
	data := []credentialEntry{}
	names := make([]string, 0, len(rt.cfg.Providers))
	for name := range rt.cfg.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		e := credentialEntry{Provider: name, Type: rt.cfg.Providers[name].Type, Supported: rt.router.CheckCredential(name) == nil}
		if i := slices.IndexFunc(rows, func(c *store.TeamCredential) bool { return c.Provider == name }); i >= 0 {
			at := rows[i].UpdatedAt
			e.Set, e.UpdatedAt, e.UpdatedBy = true, &at, rows[i].UpdatedBy
		}
		data = append(data, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
	return nil
}

func (s *Server) putTeamCredential(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := decode(r, &body); err != nil {
		return err
	}
	ctx := r.Context()
	t, err := s.store.GetTeam(ctx, r.PathValue("team_id"))
	if err != nil {
		return err
	}
	if err := write(r, t.OrgID); err != nil {
		return err
	}
	name := r.PathValue("provider")
	if err := s.rt.Load().router.CheckCredential(name); err != nil {
		return badRequest("%v", err)
	}
	if body.APIKey == "" {
		return badRequest("api_key is required")
	}
	s.cfgMu.Lock()
	sc := s.file.Secrets
	s.cfgMu.Unlock()
	box, err := secretBox(sc)
	if err != nil {
		return badRequest("storing credentials needs secrets.encryption_key in the config file: %s", err)
	}
	c := &store.TeamCredential{TeamID: t.ID, Provider: name, Secret: box.Seal([]byte(body.APIKey), credentialAD(t.ID, name)), UpdatedBy: operatorFrom(ctx).actor()}
	if err := s.store.PutTeamCredential(ctx, c); err != nil {
		return err
	}
	if err := s.audit(ctx, "team.credential.set", "team", t.ID, t.OrgID, map[string]any{"provider": name}); err != nil {
		return err
	}
	if err := s.loadTeamCredentials(ctx); err != nil {
		return err
	}
	at := c.UpdatedAt
	writeJSON(w, http.StatusOK, credentialEntry{Provider: name, Type: s.rt.Load().cfg.Providers[name].Type, Set: true, Supported: true, UpdatedAt: &at, UpdatedBy: c.UpdatedBy})
	return nil
}

func (s *Server) deleteTeamCredential(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	t, err := s.store.GetTeam(ctx, r.PathValue("team_id"))
	if err != nil {
		return err
	}
	if err := write(r, t.OrgID); err != nil {
		return err
	}
	name := r.PathValue("provider")
	if err := s.store.DeleteTeamCredential(ctx, t.ID, name); err != nil {
		return err
	}
	if err := s.audit(ctx, "team.credential.delete", "team", t.ID, t.OrgID, map[string]any{"provider": name}); err != nil {
		return err
	}
	if err := s.loadTeamCredentials(ctx); err != nil {
		return fmt.Errorf("reloading team credentials: %w", err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
