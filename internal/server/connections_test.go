package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// userUpstream is a fake upstream that is both an MCP server and its own
// OAuth authorization server. Its echo tool answers with the account the
// access token belongs to, and it refuses requests without a valid token.
type userUpstream struct {
	*httptest.Server
	t *testing.T

	mu        sync.Mutex
	account   string            // the account the next consent signs in as
	expiresIn int               // seconds, for new access tokens
	access    map[string]string // access token → account
	refresh   map[string]string // refresh token → account
	codes     map[string][2]string
	refreshes int
	revoked   []string
	n         int
}

func newUserUpstream(t *testing.T) *userUpstream {
	u := &userUpstream{t: t, expiresIn: 3600, access: map[string]string{}, refresh: map[string]string{}, codes: map[string][2]string{}}
	s := mcp.NewServer(&mcp.Implementation{Name: "gh", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "whoami"}, func(ctx context.Context, req *mcp.CallToolRequest, _ echoIn) (*mcp.CallToolResult, any, error) {
		who := req.Extra.Header.Get("X-Account")
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "gh:" + who}}}, nil, nil
	})
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{Stateless: true})
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		who, ok := u.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		u.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+u.URL+`/.well-known/oauth-protected-resource/mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Header.Set("X-Account", who)
		mcpHandler.ServeHTTP(w, r)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": u.URL + "/mcp", "authorization_servers": []string{u.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": u.URL, "authorization_endpoint": u.URL + "/authorize", "token_endpoint": u.URL + "/token", "revocation_endpoint": u.URL + "/revoke",
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "gw-client" || q.Get("code_challenge_method") != "S256" || q.Get("resource") != u.URL+"/mcp" || q.Get("scope") != "repo" {
			t.Errorf("authorize request: %v", q)
		}
		u.mu.Lock()
		u.n++
		code := fmt.Sprintf("code-%d", u.n)
		u.codes[code] = [2]string{u.account, q.Get("code_challenge")}
		u.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, _ := r.BasicAuth()
		if id != "gw-client" || secret != "gw-secret" {
			_ = r.ParseForm()
			if r.PostForm.Get("client_id") != "gw-client" || r.PostForm.Get("client_secret") != "gw-secret" {
				http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
				return
			}
		}
		_ = r.ParseForm()
		u.mu.Lock()
		defer u.mu.Unlock()
		var who string
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			c, ok := u.codes[r.PostForm.Get("code")]
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != c[1] {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			delete(u.codes, r.PostForm.Get("code"))
			who = c[0]
		case "refresh_token":
			var ok bool
			if who, ok = u.refresh[r.PostForm.Get("refresh_token")]; !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			u.refreshes++
		}
		u.n++
		at, rt := fmt.Sprintf("at-%s-%d", who, u.n), fmt.Sprintf("rt-%s-%d", who, u.n)
		u.access[at], u.refresh[rt] = who, who
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "refresh_token": rt, "token_type": "Bearer", "expires_in": u.expiresIn, "scope": "repo"})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		u.mu.Lock()
		u.revoked = append(u.revoked, r.PostForm.Get("token"))
		delete(u.refresh, r.PostForm.Get("token"))
		u.mu.Unlock()
	})
	u.Server = httptest.NewServer(mux)
	t.Cleanup(u.Close)
	return u
}

func (u *userUpstream) set(f func(u *userUpstream)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f(u)
}

// revokeAll forgets an account's tokens, as if its user revoked the app.
func (u *userUpstream) revokeAll(account string, refreshToo bool) {
	u.set(func(u *userUpstream) {
		for tok, who := range u.access {
			if who == account {
				delete(u.access, tok)
			}
		}
		if refreshToo {
			for tok, who := range u.refresh {
				if who == account {
					delete(u.refresh, tok)
				}
			}
		}
	})
}

var connectLink = regexp.MustCompile(`https?://[^\s]+/mcp/connect/[A-Za-z0-9_-]+`)

func TestMCPUserCredentials(t *testing.T) {
	up := newUserUpstream(t)
	is := newFakeIssuer(t)
	e := newAdminEnv(t, "")
	gw := httptest.NewServer(e.h)
	t.Cleanup(gw.Close)
	var org, team idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"search"}`, 201, &team)
	cfg, err := config.Parse([]byte(`
admin: {token: ` + adminToken + `}
store: {driver: sqlite, dsn: unused}
secrets: {encryption_key: "` + testEncryptionKey + `"}
mcp:
  public_url: ` + gw.URL + `
  oauth: {issuer: ` + is.URL + `, organization: ` + org.ID + `, team_claim: groups}
  servers:
    gh: {url: "` + up.URL + `/mcp", user_auth: {client_id: gw-client, client_secret: gw-secret, scopes: [repo]}}
  virtual_servers:
    dev: {servers: [{server: gh}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.srv.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	e.srv.mcp.Refresh(ctx)
	resource := gw.URL + "/mcp/dev"
	client := func(sub string) *mcp.ClientSession {
		return mcpClient(t, resource, is.token(t, resource, func(c jwt.MapClaims) { c["sub"] = sub }), nil)
	}
	alice := client("alice-sub")

	// Nobody has connected the server, so it cannot be listed; a connect
	// tool stands in for its tools.
	tools, err := alice.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "gh_connect" {
		t.Fatalf("tools before anyone connects: %+v, %v", tools, err)
	}
	res, err := alice.CallTool(ctx, &mcp.CallToolParams{Name: "gh_connect"})
	if err != nil || !res.IsError {
		t.Fatalf("connect tool: %+v, %v", res, err)
	}
	link := connectLink.FindString(text(t, res.Content))

	// The browser follows the link through the upstream's consent page.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	open := func(link string) (int, string) {
		resp, err := browser.Get(link)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	up.set(func(u *userUpstream) { u.account = "alice" })
	if code, body := open(link); code != 200 || !strings.Contains(body, "Your gh account is connected") {
		t.Fatalf("connect flow: %d %s", code, body)
	}
	if code, _ := open(link); code != http.StatusGone {
		t.Fatalf("a used link answered %d", code)
	}

	// The server is listed with alice's token, and her calls use it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		tools, _ = alice.ListTools(ctx, nil)
		if len(tools.Tools) == 1 && tools.Tools[0].Name == "gh_whoami" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tools after connecting: %+v", tools)
		}
		time.Sleep(50 * time.Millisecond)
	}
	whoami := func(cs *mcp.ClientSession) (*mcp.CallToolResult, error) {
		return cs.CallTool(ctx, &mcp.CallToolParams{Name: "gh_whoami"})
	}
	if res, err := whoami(alice); err != nil || text(t, res.Content) != "gh:alice" {
		t.Fatalf("alice's call: %+v, %v", res, err)
	}

	// Bob has not connected: clients on 2025-11-25 or later get URL
	// elicitation, older ones a tool error with the link.
	bob := client("bob-sub")
	_, err = whoami(bob)
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeURLElicitationRequired || !connectLink.MatchString(rpcErr.Message) {
		t.Fatalf("bob's first call: %v", err)
	}
	var data struct {
		Elicitations []mcp.ElicitParams `json:"elicitations"`
	}
	if json.Unmarshal(rpcErr.Data, &data) != nil || len(data.Elicitations) != 1 || data.Elicitations[0].Mode != "url" || !connectLink.MatchString(data.Elicitations[0].URL) {
		t.Fatalf("elicitation data: %s", rpcErr.Data)
	}
	oldClient := mcpClient(t, resource, is.token(t, resource, func(c jwt.MapClaims) { c["sub"] = "bob-sub" }), http.Header{"Mcp-Protocol-Version": {"2025-06-18"}})
	res, err = whoami(oldClient)
	if err != nil || !res.IsError || !connectLink.MatchString(text(t, res.Content)) {
		t.Fatalf("bob's call on an old protocol: %+v, %v", res, err)
	}
	up.set(func(u *userUpstream) { u.account = "bob" })
	if code, _ := open(data.Elicitations[0].URL); code != 200 {
		t.Fatalf("bob's connect flow: %d", code)
	}
	if res, err := whoami(bob); err != nil || text(t, res.Content) != "gh:bob" {
		t.Fatalf("bob's call: %+v, %v", res, err)
	}

	// Tokens are encrypted at rest.
	conn, err := e.srv.store.GetMCPConnection(ctx, "gh", "alice-sub")
	if err != nil || strings.Contains(conn.AccessToken, "at-") || strings.Contains(conn.RefreshToken, "rt-") || conn.ExpiresAt == nil {
		t.Fatalf("stored connection: %+v, %v", conn, err)
	}

	// The upstream refuses alice's access token: the gateway refreshes it
	// and sends the request again.
	up.revokeAll("alice", false)
	e.srv.conns.forget(connKey{"gh", "alice-sub"})
	if res, err := whoami(alice); err != nil || text(t, res.Content) != "gh:alice" {
		t.Fatalf("call after the upstream refused the token: %+v, %v", res, err)
	}

	// Tokens about to expire are refreshed before the call.
	up.set(func(u *userUpstream) { u.expiresIn = 30; u.refreshes = 0 })
	up.revokeAll("alice", false)
	e.srv.conns.forget(connKey{"gh", "alice-sub"})
	for range 2 {
		if res, err := whoami(alice); err != nil || text(t, res.Content) != "gh:alice" {
			t.Fatalf("call with an expiring token: %+v, %v", res, err)
		}
	}
	up.set(func(u *userUpstream) {
		if u.refreshes < 2 {
			t.Errorf("refreshes = %d, want one per call while the token expires within a minute", u.refreshes)
		}
	})

	// A refresh token the upstream no longer accepts ends the connection,
	// and the next call starts a new connect flow.
	up.revokeAll("bob", true)
	e.srv.conns.forget(connKey{"gh", "bob-sub"})
	if _, err := whoami(bob); !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeURLElicitationRequired {
		t.Fatalf("call after invalid_grant: %v", err)
	}
	if _, err := e.srv.store.GetMCPConnection(ctx, "gh", "bob-sub"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("connection kept after invalid_grant: %v", err)
	}

	// Gateway keys have no user, so they cannot use the server.
	var key idResp
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"ci"}`, 201, &key)
	if _, err := whoami(mcpClient(t, resource, key.Key, nil)); !errors.As(err, &rpcErr) || !strings.Contains(rpcErr.Message, "acts for each user") {
		t.Fatalf("call with a gateway key: %v", err)
	}

	// Owners list and revoke connections.
	var conns struct {
		Data []store.MCPConnection `json:"data"`
	}
	e.call("GET", "/admin/v1/mcp/connections?server=gh", "", 200, &conns)
	if len(conns.Data) != 1 || conns.Data[0].Subject != "alice-sub" || conns.Data[0].Scopes[0] != "repo" {
		t.Fatalf("connections: %+v", conns.Data)
	}
	raw := do(e.h, "GET", "/admin/v1/mcp/connections", adminToken, "").Body.String()
	if strings.Contains(raw, "token") {
		t.Fatalf("the connection list mentions tokens: %s", raw)
	}
	e.call("DELETE", "/admin/v1/mcp/connections/gh/alice-sub", "", 204, nil)
	up.set(func(u *userUpstream) {
		if len(u.revoked) != 1 || !strings.HasPrefix(u.revoked[0], "rt-alice-") {
			t.Errorf("revoked at the upstream: %v", u.revoked)
		}
	})
	if _, err := whoami(alice); !errors.As(err, &rpcErr) || rpcErr.Code != mcp.CodeURLElicitationRequired {
		t.Fatalf("call after revocation: %v", err)
	}
	audit, _, _ := e.srv.store.ListAudit(ctx, "", store.Page{Limit: 50})
	actions := map[string]int{}
	for _, a := range audit {
		actions[a.Action]++
	}
	if actions["mcp.connection.create"] != 2 || actions["mcp.connection.delete"] != 1 {
		t.Fatalf("audit actions: %v", actions)
	}
}

func TestMCPConnectSubjectCheck(t *testing.T) {
	e := newAdminEnv(t, "")
	ctx := context.Background()
	id := "link-id"
	if err := e.srv.store.CreateConnectRequest(ctx, &store.ConnectRequest{
		IDHash: hashID(id), Server: "gh", Subject: "alice-sub", Verifier: "v", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	e.srv.connectSignedIn(rec, httptest.NewRequest("GET", "/ui/auth/callback", nil), id, "mallory-sub")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "belongs to someone else") {
		t.Fatalf("another user's sign-in: %d %s", rec.Code, rec.Body)
	}
	// The link still works for its owner.
	if _, err := e.srv.store.GetConnectRequest(ctx, hashID(id), time.Now()); err != nil {
		t.Fatalf("the request was consumed: %v", err)
	}
}
