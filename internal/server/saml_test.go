package server

import (
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
