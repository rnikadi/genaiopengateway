package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

type principalKey struct{}

// principalFrom returns a principal the admin API resolved for the
// request, such as the playground's; model requests otherwise
// authenticate with an API key.
func principalFrom(ctx context.Context) (*auth.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*auth.Principal)
	return p, ok
}

// playgroundChat lets an operator call a model as a team, with the
// team's policies, through the same handler that serves API keys
// (docs/playground.md). It spends the team's budget, so it needs write
// access to the team's organization.
func (s *Server) playgroundChat(w http.ResponseWriter, r *http.Request) error {
	teamID := r.URL.Query().Get("team_id")
	if teamID == "" {
		return badRequest("team_id is required")
	}
	ctx := r.Context()
	team, err := s.store.GetTeam(ctx, teamID)
	if err != nil {
		return err
	}
	if err := write(r, team.OrgID); err != nil {
		return err
	}
	org, err := s.store.GetOrg(ctx, team.OrgID)
	if err != nil {
		return err
	}
	details, _ := json.Marshal(map[string]string{"request_id": r.Header.Get("X-Request-Id")})
	if err := s.store.AppendAudit(ctx, &store.AuditEntry{
		Actor: operatorFrom(ctx).actor(), Action: "playground.chat", ResourceType: "team", ResourceID: team.ID, OrgID: org.ID, Details: details,
	}); err != nil {
		return err
	}
	p := &auth.Principal{
		OrgID: org.ID, TeamID: team.ID, KeyName: "playground",
		Policies: [3]policy.Policy{org.Policy, team.Policy, {}},
	}
	s.model(provider.ChatCompletions)(w, r.WithContext(context.WithValue(ctx, principalKey{}, p)))
	return nil
}
