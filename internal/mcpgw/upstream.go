package mcpgw

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// upstream is one upstream MCP server: a client session shared by every
// caller, or one per user for a user_auth server, and the catalog last
// listed from it.
type upstream struct {
	name   string
	cfg    config.MCPServer
	client *mcp.Client
	base   http.RoundTripper
	http   *http.Client
	// creds and maxUsers serve user_auth servers.
	creds    Credentials
	maxUsers int

	mu      sync.Mutex
	session *mcp.ClientSession
	users   map[string]*userSession

	catalog atomic.Pointer[catalog]
	up      atomic.Bool
}

// catalog is everything an upstream server offers.
type catalog struct {
	Tools     []*mcp.Tool             `json:"tools"`
	Prompts   []*mcp.Prompt           `json:"prompts"`
	Resources []*mcp.Resource         `json:"resources"`
	Templates []*mcp.ResourceTemplate `json:"resourceTemplates"`

	protocolVersion string
	refreshedAt     time.Time
	// hidden holds the names of tools that await approval.
	hidden map[string]bool
	// hash identifies the content and hidden set, to detect changes.
	hash [32]byte
}

// seal computes the catalog's hash once its hidden set is known.
func (c *catalog) seal() {
	raw, _ := json.Marshal(c)
	hidden := slices.Sorted(maps.Keys(c.hidden))
	extra, _ := json.Marshal(hidden)
	c.hash = sha256.Sum256(append(raw, extra...))
}

func newUpstream(name string, cfg config.MCPServer, base http.RoundTripper, creds Credentials, maxUsers int) *upstream {
	return &upstream{
		name:     name,
		cfg:      cfg,
		client:   mcp.NewClient(&mcp.Implementation{Name: "genaiopengateway", Version: Version}, nil),
		base:     base,
		http:     &http.Client{Transport: &headerTransport{base: base, headers: cfg.Headers}},
		creds:    creds,
		maxUsers: max(maxUsers, 1),
		users:    map[string]*userSession{},
	}
}

// perUser reports whether calls use each user's own credentials.
func (u *upstream) perUser() bool { return u.cfg.UserAuth != nil && u.creds != nil }

// headerTransport adds the credentials configured for an upstream.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]config.Secret
}

func (t *headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(t.headers) > 0 {
		r = r.Clone(r.Context())
		for k, v := range t.headers {
			r.Header.Set(k, v.Value())
		}
	}
	return t.base.RoundTrip(r)
}

// connect returns the shared session, opening it if needed.
func (u *upstream) connect(ctx context.Context) (*mcp.ClientSession, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.session != nil {
		return u.session, nil
	}
	// The gateway only sends requests, so it needs no stream for
	// server-initiated messages.
	t := &mcp.StreamableClientTransport{Endpoint: u.cfg.URL, HTTPClient: u.http, DisableStandaloneSSE: true, MaxRetries: -1}
	cs, err := u.client.Connect(ctx, t, nil)
	if err != nil {
		return nil, err
	}
	u.session = cs
	return cs, nil
}

// drop forgets a broken session so the next call opens a new one.
func (u *upstream) drop(cs *mcp.ClientSession) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.session == cs {
		u.session = nil
		go cs.Close()
	}
}

func (u *upstream) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.session != nil {
		_ = u.session.Close()
		u.session = nil
	}
	for subject := range u.users {
		u.closeUserLocked(subject)
	}
}

// do runs f within the server's timeout on the session of a user, or on
// the shared session when subject is empty. A broken session is
// replaced. When the request was never delivered, because the session
// had already closed or the upstream no longer knew it, f is retried
// once on a new session.
func (u *upstream) do(ctx context.Context, subject string, f func(context.Context, *mcp.ClientSession) error) error {
	ctx, cancel := context.WithTimeout(ctx, u.cfg.Timeout.Std())
	defer cancel()
	for attempt := 0; ; attempt++ {
		var cs *mcp.ClientSession
		var err error
		if subject == "" {
			cs, err = u.connect(ctx)
		} else {
			cs, err = u.userSession(ctx, subject)
		}
		if err != nil {
			return err
		}
		err = f(ctx, cs)
		if brokenSession(err) {
			if subject == "" {
				u.drop(cs)
			} else {
				u.dropUser(subject, cs)
			}
			if attempt == 0 {
				continue
			}
		}
		return err
	}
}

// errNoUser is returned when a user_auth server cannot be listed because
// nobody has connected it, and it was listed before.
var errNoUser = errors.New("no user has connected this server")

// list fetches the upstream's catalog, following pagination. A user_auth
// server is listed with the token of the user who used it last; until
// someone connects it, it is listed anonymously.
func (u *upstream) list(ctx context.Context) (*catalog, error) {
	subject := ""
	if u.perUser() {
		if subject = u.creds.RecentSubject(ctx, u.name); subject == "" && u.catalog.Load() != nil {
			return nil, errNoUser
		}
	}
	var c *catalog
	err := u.do(ctx, subject, func(ctx context.Context, cs *mcp.ClientSession) error {
		c = &catalog{Tools: []*mcp.Tool{}, Prompts: []*mcp.Prompt{}, Resources: []*mcp.Resource{}, Templates: []*mcp.ResourceTemplate{}}
		init := cs.InitializeResult()
		c.protocolVersion = init.ProtocolVersion
		caps := init.Capabilities
		if caps == nil {
			caps = &mcp.ServerCapabilities{}
		}
		if caps.Tools != nil {
			for t, err := range cs.Tools(ctx, nil) {
				if err != nil {
					return err
				}
				c.Tools = append(c.Tools, t)
			}
		}
		if caps.Prompts != nil {
			for p, err := range cs.Prompts(ctx, nil) {
				if err != nil {
					return err
				}
				c.Prompts = append(c.Prompts, p)
			}
		}
		if caps.Resources != nil {
			for r, err := range cs.Resources(ctx, nil) {
				if err != nil {
					return err
				}
				c.Resources = append(c.Resources, r)
			}
			for t, err := range cs.ResourceTemplates(ctx, nil) {
				if isMethodNotFound(err) {
					break // templates are optional
				}
				if err != nil {
					return err
				}
				c.Templates = append(c.Templates, t)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.refreshedAt = time.Now()
	return c, nil
}

func isMethodNotFound(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeMethodNotFound
}

// brokenSession reports a request that was never delivered: the
// upstream no longer knew the session, or the session had closed before
// the request was sent. (The SDK reports a session that breaks while a
// request is in flight with the underlying error instead.)
func brokenSession(err error) bool {
	return errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed)
}
