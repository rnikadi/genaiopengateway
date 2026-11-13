package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// The self-service connections page (docs/mcp-user-credentials.md): a
// user signs in with the identity provider that issues MCP tokens, sees
// the upstream accounts they connected and disconnects any of them. It
// needs no role on the gateway, like the connect flow, and starts no
// admin session: a short signed cookie remembers who signed in.

const (
	myConnectionsPath = "/mcp/connections"
	// myConnCookie holds the signed-in subject for the page.
	myConnCookie = "gog_mcp_me"
	myConnTTL    = 15 * time.Minute
)

// myConn is what the page's cookie says: who signed in, a token that
// forms must echo, and when it stops working.
type myConn struct {
	Subject string `json:"s"`
	Email   string `json:"m,omitempty"`
	CSRF    string `json:"c"`
	Expires int64  `json:"e"`
}

// myConnMAC signs the cookie with a key every replica has: the one that
// signs OIDC sign-in flows, or with SAML one derived from the key that
// encrypts stored secrets, which user_auth servers require.
func myConnMAC(cfg *config.Config, raw []byte) []byte {
	var key []byte
	if verifySubject(cfg) == viaOIDC {
		key = flowKey(cfg.SSO.OIDC)
	} else {
		sum := sha256.Sum256([]byte("genaiopengateway mcp connections\x00" + cfg.Secrets.EncryptionKey.Value()))
		key = sum[:]
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("mcp connections\x00"))
	m.Write(raw)
	return m.Sum(nil)
}

func sealMyConn(cfg *config.Config, c *myConn) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(myConnMAC(cfg, raw))
}

func openMyConn(cfg *config.Config, v string) (*myConn, error) {
	body, sig, ok := strings.Cut(v, ".")
	raw, err1 := base64.RawURLEncoding.DecodeString(body)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if !ok || err1 != nil || err2 != nil || !hmac.Equal(mac, myConnMAC(cfg, raw)) {
		return nil, errors.New("invalid connections cookie")
	}
	var c myConn
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if time.Now().Unix() > c.Expires || c.Subject == "" {
		return nil, errors.New("connections cookie expired")
	}
	return &c, nil
}

// userAuthServers names the upstreams users connect their own accounts to.
func userAuthServers(cfg *config.Config) []string {
	var out []string
	for name, sc := range cfg.MCP.Servers {
		if sc.UserAuth != nil {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// myConnectionsURL is the page's address, or empty when the gateway
// cannot tell who a user is or has no per-user upstreams.
func myConnectionsURL(cfg *config.Config) string {
	if verifySubject(cfg) == "" || len(userAuthServers(cfg)) == 0 {
		return ""
	}
	return strings.TrimRight(cfg.MCP.PublicURL, "/") + myConnectionsPath
}

// myConnections shows the signed-in user's connections, signing them in
// first.
func (s *Server) myConnections(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg
	if verifySubject(cfg) == "" {
		connectPage(w, http.StatusNotFound, "This page is not set up",
			"Seeing your own MCP connections needs the gateway's sign-in to use the identity provider that issues MCP tokens, or with SAML the attribute that carries their subject. Ask an administrator to remove a connection for you.")
		return
	}
	me, err := openMyConn(cfg, readCookie(r, myConnCookie))
	if err != nil && verifySubject(cfg) == viaSAML {
		s.samlStart(w, r, cfg.SSO.SAML, samlFlow{Return: "/ui/", Mine: true})
		return
	}
	if err != nil {
		c, err := s.oidc(r.Context(), cfg.SSO.OIDC)
		if err != nil {
			s.log.Error("reaching the identity provider failed", "err", err)
			connectPage(w, http.StatusBadGateway, "The identity provider cannot be reached", "Try again shortly.")
			return
		}
		s.signIn(w, r, cfg.SSO.OIDC, c, &authFlow{Return: "/ui/", Mine: true})
		return
	}
	conns, _, err := s.store.ListMCPConnections(r.Context(), "", me.Subject, store.Page{Limit: 500})
	if err != nil {
		s.log.Error("listing MCP connections failed", "err", err)
		connectPage(w, http.StatusInternalServerError, "Your connections cannot be read", "Try again shortly.")
		return
	}
	connected := map[string]bool{}
	for _, c := range conns {
		connected[c.Server] = true
	}
	var other []string
	for _, name := range userAuthServers(cfg) {
		if !connected[name] {
			other = append(other, name)
		}
	}
	who := me.Email
	if who == "" {
		who = me.Subject
	}
	notice := ""
	if d := r.URL.Query().Get("disconnected"); d != "" && !connected[d] {
		notice = "Disconnected " + d + ". The gateway no longer holds a token for your account there."
	}
	myConnPage(w, http.StatusOK, myConnView{Who: who, Conns: conns, Other: other, CSRF: me.CSRF, Notice: notice})
}

// mySignedIn finishes the sign-in for the page.
func (s *Server) mySignedIn(w http.ResponseWriter, r *http.Request, cfg *config.Config, subject, email string) {
	me := &myConn{Subject: subject, Email: email, CSRF: randomString(), Expires: time.Now().Add(myConnTTL).Unix()}
	setCookie(w, r, myConnCookie, sealMyConn(cfg, me), myConnTTL)
	http.Redirect(w, r, myConnectionsPath, http.StatusFound)
}

// disconnectMine removes one of the signed-in user's connections,
// revoking it at the upstream when the upstream allows.
func (s *Server) disconnectMine(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg
	if verifySubject(cfg) == "" {
		http.NotFound(w, r)
		return
	}
	me, err := openMyConn(cfg, readCookie(r, myConnCookie))
	if err != nil {
		connectPage(w, http.StatusUnauthorized, "Your sign-in has expired", "Open your connections page again to sign in.")
		return
	}
	scheme := "http"
	if secure(r) {
		scheme = "https"
	}
	if r.Header.Get("Origin") != scheme+"://"+r.Host || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(me.CSRF)) != 1 {
		connectPage(w, http.StatusForbidden, "This request came from another page", "Open your connections page again and use its button.")
		return
	}
	server := r.PathValue("server")
	if err := s.removeConnection(r.Context(), server, me.Subject, me.Subject, "user"); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, myConnectionsPath, http.StatusSeeOther)
			return
		}
		s.log.Error("removing an MCP connection failed", "server", server, "err", err)
		connectPage(w, http.StatusInternalServerError, "The connection could not be removed", "Try again shortly.")
		return
	}
	http.Redirect(w, r, myConnectionsPath+"?disconnected="+template.URLQueryEscaper(server), http.StatusSeeOther)
}

// removeConnection revokes a connection at the upstream when it can,
// deletes it and audits who did it.
func (s *Server) removeConnection(ctx context.Context, server, subject, actor, by string) error {
	conn, err := s.store.GetMCPConnection(ctx, server, subject)
	if err != nil {
		return err
	}
	ctx = context.WithoutCancel(ctx)
	s.conns.revoke(ctx, server, conn)
	if err := s.store.DeleteMCPConnection(ctx, server, subject); err != nil {
		return err
	}
	s.conns.forget(connKey{server, subject})
	details, _ := json.Marshal(map[string]any{"server": server, "subject": subject, "by": by})
	return s.store.AppendAudit(ctx, &store.AuditEntry{
		Actor: actor, Action: "mcp.connection.delete", ResourceType: "mcp_connection", ResourceID: server + "/" + subject, Details: details,
	})
}

type myConnView struct {
	Who    string
	Conns  []*store.MCPConnection
	Other  []string
	CSRF   string
	Notice string
}

var myConnTmpl = template.Must(template.New("mine").Funcs(template.FuncMap{
	"date": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.UTC().Format("2 Jan 2006 15:04 UTC")
	},
	"join": strings.Join,
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Your MCP connections</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:0;background:#f5f6f8;color:#1d2330}
main{background:#fff;border:1px solid #e3e6eb;border-radius:10px;padding:24px 28px;max-width:720px;margin:32px auto}
h1{font-size:22px;margin:0 0 4px}h2{font-size:16px;margin:24px 0 8px}p{margin:0 0 8px;color:#525b6b}
.notice{background:#e8f5ee;color:#1d5c3a;border-radius:6px;padding:8px 12px}
table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:8px 6px;border-bottom:1px solid #e3e6eb;vertical-align:top}
th{font-size:13px;color:#525b6b;font-weight:600}small{color:#525b6b}
button{font:inherit;font-size:14px;padding:6px 12px;border-radius:6px;border:1px solid #c4382d;background:#fff;color:#a02a20;cursor:pointer}
button:focus-visible{outline:2px solid #2f6fdf;outline-offset:2px}
@media (max-width:560px){main{margin:0;border-radius:0;border-width:0 0 1px;padding:20px 16px}th:nth-child(3),td:nth-child(3){display:none}}
@media (prefers-color-scheme:dark){body{background:#14171c;color:#e6e8eb}main{background:#1c2027;border-color:#2c313a}p,th,small{color:#a3abb8}
th,td{border-color:#2c313a}.notice{background:#173426;color:#9fe0bb}button{background:#1c2027;color:#ff9b91;border-color:#ff9b91}}</style>
</head><body><main>
<h1>Your MCP connections</h1>
<p>Signed in as <strong>{{.Who}}</strong>. These are the accounts you connected so the gateway can call MCP servers as you. Disconnecting deletes the gateway's token and revokes it at the server when the server allows; your MCP client asks you to connect again the next time you use that server.</p>
{{if .Notice}}<p class="notice" role="status">{{.Notice}}</p>{{end}}
<h2>Connected</h2>
{{if .Conns}}<table><thead><tr><th scope="col">Server</th><th scope="col">Since</th><th scope="col">Last used</th><th scope="col">Action</th></tr></thead><tbody>
{{range .Conns}}<tr><td><strong>{{.Server}}</strong>{{if .Scopes}}<br><small>Access: {{join .Scopes ", "}}</small>{{end}}</td><td>{{date .CreatedAt}}</td><td>{{date .LastUsedAt}}</td>
<td><form method="post" action="/mcp/connections/{{.Server}}/disconnect"><input type="hidden" name="csrf" value="{{$.CSRF}}"><button type="submit" aria-label="Disconnect {{.Server}}">Disconnect</button></form></td></tr>
{{end}}</tbody></table>{{else}}<p>You have not connected any account yet.</p>{{end}}
{{if .Other}}<h2>Not connected</h2><p>{{join .Other ", "}}. Your MCP client gives you a link to connect the first time you use one of these.</p>{{end}}
</main></body></html>`))

func myConnPage(w http.ResponseWriter, status int, v myConnView) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = myConnTmpl.Execute(w, v)
}
