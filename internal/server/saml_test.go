package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"html"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"golang.org/x/oauth2"
)

// fakeSAMLIdP is a SAML identity provider that signs in whoever user is.
type fakeSAMLIdP struct {
	idp     *saml.IdentityProvider
	srv     *httptest.Server
	gateway http.Handler
	user    *saml.Session
	// noEncrypt hides the gateway's encryption key from the identity
	// provider, so that it sends assertions in the clear.
	noEncrypt bool
	// participants and loggedOut are the identity provider's view of
	// single logout.
	participants []saml.SessionParticipant
	loggedOut    int
}

func (f *fakeSAMLIdP) InvalidateSession(http.ResponseWriter, *http.Request, *saml.IdpLogoutRequest) error {
	f.loggedOut++
	return nil
}

func (f *fakeSAMLIdP) AddSessionParticipant(_ *http.Request, _ *saml.Session, p saml.SessionParticipant) error {
	f.participants = append(f.participants[:0], p)
	return nil
}

func (f *fakeSAMLIdP) SessionParticipants(*http.Request, func(*saml.Session) bool) ([]saml.SessionParticipant, error) {
	return f.participants, nil
}

func (f *fakeSAMLIdP) GetSession(http.ResponseWriter, *http.Request, *saml.IdpAuthnRequest) *saml.Session {
	return f.user
}

// GetServiceProvider reads the gateway's own metadata, as an identity
// provider does when the gateway is registered with it.
func (f *fakeSAMLIdP) GetServiceProvider(_ *http.Request, _ string) (*saml.EntityDescriptor, error) {
	rec := httptest.NewRecorder()
	f.gateway.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/auth/saml/metadata", nil))
	if rec.Code != 200 {
		log.Printf("metadata: %d %s", rec.Code, rec.Body)
		return nil, os.ErrNotExist
	}
	md, err := samlsp.ParseMetadata(rec.Body.Bytes())
	if err != nil {
		log.Printf("metadata: %v\n%s", err, rec.Body)
	}
	if f.noEncrypt && md != nil {
		for i := range md.SPSSODescriptors {
			d := &md.SPSSODescriptors[i]
			d.KeyDescriptors = slices.DeleteFunc(d.KeyDescriptors, func(k saml.KeyDescriptor) bool { return k.Use == "encryption" })
		}
	}
	return md, err
}

func newFakeSAMLIdP(t *testing.T) *fakeSAMLIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "idp"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	f := &fakeSAMLIdP{}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	base, _ := url.Parse(f.srv.URL)
	f.idp = &saml.IdentityProvider{
		Key: key, Certificate: cert,
		MetadataURL: *base.JoinPath("metadata"), SSOURL: *base.JoinPath("sso"),
		ServiceProviderProvider: f, SessionProvider: f, Logger: log.New(os.Stderr, "idp: ", 0),
	}
	mux.HandleFunc("/metadata", f.idp.ServeMetadata)
	mux.HandleFunc("/sso", f.idp.ServeSSO)
	return f
}

var samlFormValue = regexp.MustCompile(`name="SAMLResponse" value="([^"]+)"`)

// signIn runs the browser's part: start at the gateway, follow it to the
// identity provider, and post the response back. It returns the reply
// to the post, and the flow cookie used.
func (f *fakeSAMLIdP) signIn(t *testing.T, flow string) (*httptest.ResponseRecorder, *http.Cookie, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.gateway.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/auth/saml/login?return=/ui/%23/keys", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == samlCookie {
			cookie = c
		}
	}
	if flow != "" {
		cookie.Value = flow
	}
	idpRec := httptest.NewRecorder()
	f.idp.ServeSSO(idpRec, httptest.NewRequest("GET", rec.Header().Get("Location"), nil))
	m := samlFormValue.FindStringSubmatch(idpRec.Body.String())
	if m == nil {
		t.Fatalf("the identity provider did not answer with a form: %d %s", idpRec.Code, idpRec.Body)
	}
	response := html.UnescapeString(m[1])
	return f.post(cookie, response), cookie, response
}

func (f *fakeSAMLIdP) post(cookie *http.Cookie, response string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/ui/auth/saml/acs", strings.NewReader(url.Values{"SAMLResponse": {response}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.gateway.ServeHTTP(rec, req)
	return rec
}

func TestSAMLSignIn(t *testing.T) {
	f := newFakeSAMLIdP(t)
	e := newAdminEnv(t, `
sso:
  saml:
    idp_metadata_url: "`+f.srv.URL+`/metadata"
    acs_url: "http://example.com/ui/auth/saml/acs"
  roles: [{group: gateway-owners, role: owner}]
`)
	f.gateway = e.h
	f.user = &saml.Session{
		ID: "s1", NameID: "ada@example.com", UserEmail: "ada@example.com", UserCommonName: "Ada Lovelace",
		CustomAttributes: []saml.Attribute{
			{Name: "groups", Values: []saml.AttributeValue{{Type: "xs:string", Value: "gateway-owners"}, {Type: "xs:string", Value: "staff"}}},
			{Name: "displayName", Values: []saml.AttributeValue{{Type: "xs:string", Value: "Ada Lovelace"}}},
		},
	}

	var methods map[string]bool
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/auth/config", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &methods)
	if !methods["saml"] || methods["oidc"] || methods["token"] {
		t.Fatalf("sign-in methods: %v", methods)
	}

	got, flow, response := f.signIn(t, "")
	if got.Code != http.StatusFound || got.Header().Get("Location") != "/ui/#/keys" {
		t.Fatalf("acs = %d %s: %s", got.Code, got.Header().Get("Location"), got.Body)
	}
	session := sessionFrom(t, got)
	var me struct {
		Operator struct {
			Email, Name string
			Grants      []struct{ Role string }
		}
	}
	if err := json.Unmarshal(e.browser(session, "GET", "/admin/v1/me", "").Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Operator.Email != "ada@example.com" || me.Operator.Name != "Ada Lovelace" || len(me.Operator.Grants) != 1 || me.Operator.Grants[0].Role != "owner" {
		t.Fatalf("signed in as %+v", me)
	}

	if rec := f.post(flow, response); rec.Code != http.StatusUnauthorized {
		t.Errorf("a replayed response = %d, want 401", rec.Code)
	}
	if rec, _, _ := f.signIn(t, "bm90LWEtZmxvdw"); rec.Code != http.StatusBadRequest {
		t.Errorf("a response without our flow = %d, want 400", rec.Code)
	}
	if rec, _, _ := f.signIn(t, flowFor("id-of-another-request")); rec.Code != http.StatusUnauthorized {
		t.Errorf("a response to another request = %d, want 401", rec.Code)
	}
	if rec := f.post(nil, response); rec.Code != http.StatusBadRequest {
		t.Errorf("no flow cookie = %d, want 400", rec.Code)
	}

	f.idp.SignatureMethod = "http://www.w3.org/2000/09/xmldsig#rsa-sha1"
	if rec, _, _ := f.signIn(t, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("a response signed with SHA-1 = %d, want 401", rec.Code)
	}
	f.idp.SignatureMethod = ""

	f.user.CustomAttributes = f.user.CustomAttributes[1:]
	if rec, _, _ := f.signIn(t, ""); rec.Code != http.StatusForbidden {
		t.Errorf("a user without a mapped group = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/auth/saml/metadata", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `entityID="http://example.com/ui/auth/saml/metadata"`) || !strings.Contains(rec.Body.String(), "http://example.com/ui/auth/saml/acs") {
		t.Errorf("metadata: %d %s", rec.Code, rec.Body)
	}
}

func flowFor(requestID string) string {
	raw, _ := json.Marshal(samlFlow{RequestID: requestID, Return: "/ui/"})
	return b64(raw)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// through starts at a gateway page that signs the user in with SAML,
// follows the browser to the identity provider and posts its response
// back. It returns the gateway's answer to the response.
func (f *fakeSAMLIdP) through(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	f.gateway.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), f.srv.URL+"/sso") {
		t.Fatalf("%s = %d to %s: %s", path, rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == samlCookie {
			cookie = c
		}
	}
	idpRec := httptest.NewRecorder()
	f.idp.ServeSSO(idpRec, httptest.NewRequest("GET", rec.Header().Get("Location"), nil))
	m := samlFormValue.FindStringSubmatch(idpRec.Body.String())
	if m == nil {
		t.Fatalf("the identity provider did not answer with a form: %d %s", idpRec.Code, idpRec.Body)
	}
	return f.post(cookie, html.UnescapeString(m[1]))
}

// With SAML sign-in, the MCP connect flow and connections page check
// who the user is by the attribute that carries the subject of their
// MCP tokens (docs/mcp-user-credentials.md).
func TestSAMLProvesMCPUser(t *testing.T) {
	f := newFakeSAMLIdP(t)
	up := newUserUpstream(t)
	e := newAdminEnv(t, "")
	var org idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	parse := func(attribute string) *config.Config {
		t.Helper()
		cfg, err := config.Parse([]byte(`
admin: {token: ` + adminToken + `}
store: {driver: sqlite, dsn: unused}
secrets: {encryption_key: "` + testEncryptionKey + `"}
sso:
  saml: {idp_metadata_url: "` + f.srv.URL + `/metadata", acs_url: "http://example.com/ui/auth/saml/acs"` + attribute + `}
  roles: [{group: gateway-owners, role: owner}]
mcp:
  public_url: http://example.com
  oauth: {issuer: "https://idp.example", organization: ` + org.ID + `, team_claim: groups}
  servers:
    gh: {url: "` + up.URL + `/mcp", user_auth: {client_id: gw-client, client_secret: gw-secret, scopes: [repo], authorization_url: "` + up.URL + `/authorize", token_url: "` + up.URL + `/token"}}
  virtual_servers:
    dev: {servers: [{server: gh}]}
`))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	f.gateway = e.h
	// Ada has no role on the gateway: proving who she is needs none.
	f.user = &saml.Session{ID: "s1", NameID: "ada@example.com", UserEmail: "ada@example.com", CustomAttributes: []saml.Attribute{
		{Name: "uid", Values: []saml.AttributeValue{{Type: "xs:string", Value: "ada-sub"}}},
	}}
	ctx := context.Background()

	// Without the attribute the gateway cannot tell who a user is.
	cfg := parse("")
	if err := e.srv.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	if rec := do(e.h, "GET", "/mcp/connections", "", ""); rec.Code != 404 || myConnectionsURL(cfg) != "" {
		t.Fatalf("without the attribute: %d %q", rec.Code, myConnectionsURL(cfg))
	}

	cfg = parse(`, mcp_subject_attribute: uid`)
	if err := e.srv.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"ada-sub", "bob-sub"} {
		up.set(func(u *userUpstream) { u.access["at-"+subject], u.refresh["rt-"+subject] = subject, subject })
	}
	if err := e.srv.conns.save(ctx, "gh", "bob-sub", []string{"repo"}, &oauth2.Token{AccessToken: "at-bob-sub", RefreshToken: "rt-bob-sub", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// The connections page: Ada sees her own, which are none.
	rec := f.through(t, "/mcp/connections")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/mcp/connections" {
		t.Fatalf("after sign-in: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	var me *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == myConnCookie {
			me = c
		}
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("proving who she is started an admin session")
		}
	}
	if me == nil {
		t.Fatal("no connections cookie")
	}
	req := httptest.NewRequest("GET", "/mcp/connections", nil)
	req.AddCookie(me)
	page := httptest.NewRecorder()
	e.h.ServeHTTP(page, req)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "ada@example.com") || !strings.Contains(page.Body.String(), "gh") || strings.Contains(page.Body.String(), "<button") {
		t.Fatalf("Ada's page: %d %s", page.Code, page.Body)
	}

	// A connect link works for the user it was made for, and for no other.
	link := func(subject string) string {
		u, err := e.srv.conns.ConnectURL(ctx, "gh", subject, "dev")
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimPrefix(u, "http://example.com")
	}
	if rec := f.through(t, link("ada-sub")); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), up.URL+"/authorize") {
		t.Fatalf("Ada's own link: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	if rec := f.through(t, link("bob-sub")); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "someone else") {
		t.Fatalf("Bob's link opened by Ada: %d %s", rec.Code, rec.Body)
	}

	// An assertion without the attribute proves nothing.
	f.user.CustomAttributes = nil
	if rec := f.through(t, link("ada-sub")); rec.Code != http.StatusForbidden {
		t.Fatalf("no attribute: %d %s", rec.Code, rec.Body)
	}
	// The NameID can be the subject.
	if err := e.srv.Reload(parse(`, mcp_subject_attribute: NameID`)); err != nil {
		t.Fatal(err)
	}
	if rec := f.through(t, link("ada@example.com")); rec.Code != http.StatusFound {
		t.Fatalf("NameID as the subject: %d %s", rec.Code, rec.Body)
	}
}
