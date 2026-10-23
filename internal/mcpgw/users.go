package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Per-user upstream credentials (docs/mcp-user-credentials.md): a
// user_auth upstream is called with each user's own token, over one
// session per user.

// Credentials supplies user tokens for user_auth upstreams.
type Credentials interface {
	// Token returns a user's access token for a server, refreshed when it
	// is about to expire or when force is set. It returns
	// ErrNotConnected when the user has no usable connection.
	Token(ctx context.Context, server, subject string, force bool) (string, error)
	// Broken forgets a connection the upstream no longer accepts.
	Broken(ctx context.Context, server, subject string)
	// ConnectURL starts a connect flow and returns the URL the user opens.
	ConnectURL(ctx context.Context, server, subject, virtualServer string) (string, error)
	// RecentSubject names a connected user whose token can list the
	// server's catalog, or returns "".
	RecentSubject(ctx context.Context, server string) string
}

// ErrNotConnected is returned by Credentials.Token for a user who has not
// connected the server, or whose connection stopped working.
var ErrNotConnected = errors.New("mcpgw: user has not connected this server")

// User sessions close after this long unused.
const userSessionIdle = 10 * time.Minute

// userSession is one user's session with a user_auth upstream.
type userSession struct {
	mu       sync.Mutex // serializes opening the session
	cs       *mcp.ClientSession
	lastUsed time.Time // guarded by upstream.mu
}

// userSession returns a user's session, opening it if needed. When the
// upstream holds its maximum of user sessions, the least recently used
// is closed.
func (u *upstream) userSession(ctx context.Context, subject string) (*mcp.ClientSession, error) {
	u.mu.Lock()
	us := u.users[subject]
	if us == nil {
		if len(u.users) >= u.maxUsers {
			u.evictLocked()
		}
		us = &userSession{}
		u.users[subject] = us
	}
	us.lastUsed = time.Now()
	u.mu.Unlock()

	us.mu.Lock()
	defer us.mu.Unlock()
	if us.cs != nil {
		return us.cs, nil
	}
	hc := &http.Client{Transport: &headerTransport{
		base:    &bearerTransport{base: u.base, server: u.name, subject: subject, creds: u.creds},
		headers: u.cfg.Headers,
	}}
	t := &mcp.StreamableClientTransport{Endpoint: u.cfg.URL, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1}
	cs, err := u.client.Connect(ctx, t, nil)
	if err != nil {
		return nil, err
	}
	us.cs = cs
	return cs, nil
}

// evictLocked closes the least recently used user session.
func (u *upstream) evictLocked() {
	var oldest string
	var at time.Time
	for subject, us := range u.users {
		if oldest == "" || us.lastUsed.Before(at) {
			oldest, at = subject, us.lastUsed
		}
	}
	u.closeUserLocked(oldest)
}

func (u *upstream) closeUserLocked(subject string) {
	us := u.users[subject]
	delete(u.users, subject)
	if us == nil {
		return
	}
	go func() {
		us.mu.Lock()
		defer us.mu.Unlock()
		if us.cs != nil {
			_ = us.cs.Close()
			us.cs = nil
		}
	}()
}

// dropUser forgets a user's broken session.
func (u *upstream) dropUser(subject string, cs *mcp.ClientSession) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if us := u.users[subject]; us != nil && us.cs == cs {
		u.closeUserLocked(subject)
	}
}

// reap closes user sessions unused for longer than idle.
func (u *upstream) reap(idle time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for subject, us := range u.users {
		if time.Since(us.lastUsed) > idle {
			u.closeUserLocked(subject)
		}
	}
}

// bearerTransport sends a user's access token. When the upstream answers
// 401 the token is refreshed and the request sent once more, which is
// safe because the request never reached a tool. A second 401 marks the
// connection broken.
type bearerTransport struct {
	base            http.RoundTripper
	server, subject string
	creds           Credentials
}

func (t *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, err := t.creds.Token(r.Context(), t.server, t.subject, false)
	if err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(withBearer(r, r.Body, tok))
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	if r.Body != nil && r.GetBody == nil {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if tok, err = t.creds.Token(r.Context(), t.server, t.subject, true); err != nil {
		return nil, err
	}
	var body io.ReadCloser
	if r.GetBody != nil {
		if body, err = r.GetBody(); err != nil {
			return nil, err
		}
	}
	resp, err = t.base.RoundTrip(withBearer(r, body, tok))
	if err == nil && resp.StatusCode == http.StatusUnauthorized {
		t.creds.Broken(r.Context(), t.server, t.subject)
	}
	return resp, err
}

func withBearer(r *http.Request, body io.ReadCloser, token string) *http.Request {
	r = r.Clone(r.Context())
	r.Body = body
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// errAnonymous is returned for a call to a user_auth upstream from a
// caller with no user identity: a gateway key, not an OAuth token.
func errAnonymous(server string) error {
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidRequest,
		Message: "upstream MCP server " + server + " acts for each user: sign in with your identity provider (OAuth), not a gateway API key, to use it",
	}
}

// ConnectError says a user must connect an upstream account first.
type ConnectError struct {
	Server, URL string
}

func (e *ConnectError) Error() string { return e.message() }

func (e *ConnectError) message() string {
	return "Connect your " + e.Server + " account to continue: open " + e.URL + " , then try again."
}

// rpcError is the URL elicitation error of MCP 2025-11-25. Its message
// carries the link too, for clients that only show the message.
func (e *ConnectError) rpcError() *jsonrpc.Error {
	data, _ := json.Marshal(map[string]any{"elicitations": []*mcp.ElicitParams{{
		Mode: "url", URL: e.URL, ElicitationID: e.URL, Message: "Connect your " + e.Server + " account",
	}}})
	return &jsonrpc.Error{Code: mcp.CodeURLElicitationRequired, Message: e.message(), Data: data}
}

// toolResult tells clients that predate URL elicitation, as a tool error
// every client shows.
func (e *ConnectError) toolResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: e.message()}}}
}

// urlElicitationVersion is the first protocol version with URL elicitation.
const urlElicitationVersion = "2025-11-25"
