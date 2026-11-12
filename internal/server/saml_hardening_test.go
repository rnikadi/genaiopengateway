package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// spKeyPair returns a key pair for the gateway, as one-line PEM, the way
// it arrives from an environment variable or a password field.
func spKeyPair(t *testing.T) (cert, key string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "gateway"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	pk, _ := x509.MarshalPKCS8PrivateKey(k)
	oneLine := func(b []byte) string { return strings.ReplaceAll(string(b), "\n", " ") }
	return oneLine(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), oneLine(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}))
}

// newHardenedSAML starts a gateway with its own key pair, and an identity
// provider that wants signed requests, refuses SHA-1, encrypts assertions
// and does single logout.
func newHardenedSAML(t *testing.T) (*fakeSAMLIdP, *adminEnv) {
	t.Helper()
	f := newFakeSAMLIdP(t)
	base, _ := url.Parse(f.srv.URL)
	f.idp.LogoutURL = *base.JoinPath("slo")
	f.idp.LogoutBindings = []string{saml.HTTPRedirectBinding, saml.HTTPPostBinding}
	f.idp.WantAuthnRequestsSigned = true
	f.idp.RejectSHA1Signatures = true
	cert, key := spKeyPair(t)
	e := newAdminEnv(t, `
sso:
  saml:
    idp_metadata_url: "`+f.srv.URL+`/metadata"
    acs_url: "http://example.com/ui/auth/saml/acs"
    sp_certificate: "`+cert+`"
    sp_private_key: "`+key+`"
    require_encrypted_assertions: true
  roles: [{group: gateway-owners, role: owner}]
`)
	f.gateway = e.h
	f.user = &saml.Session{
		ID: "s1", Index: "idx-1", NameID: "ada@example.com", NameIDFormat: "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress", UserEmail: "ada@example.com",
		CustomAttributes: []saml.Attribute{{Name: "groups", Values: []saml.AttributeValue{{Type: "xs:string", Value: "gateway-owners"}}}},
	}
	return f, e
}

func TestSAMLSignedRequestsAndEncryptedAssertions(t *testing.T) {
	f, e := newHardenedSAML(t)

	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/auth/saml/metadata", nil))
	md := rec.Body.String()
	for _, want := range []string{`AuthnRequestsSigned="true"`, `use="signing"`, `use="encryption"`, `aes256-gcm`, `SingleLogoutService`, `http://example.com/ui/auth/saml/slo`} {
		if !strings.Contains(md, want) {
			t.Errorf("metadata lacks %s:\n%s", want, md)
		}
	}

	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/auth/saml/login", nil))
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "Signature=") || !strings.Contains(loc, "SigAlg=") {
		t.Fatalf("the AuthnRequest is not signed: %s", loc)
	}

	// The identity provider refuses unsigned requests, so a sign-in shows
	// that it verified the gateway's signature.
	got, _, response := f.signIn(t, "")
	if got.Code != http.StatusFound {
		t.Fatalf("acs = %d: %s", got.Code, got.Body)
	}
	raw, _ := base64.StdEncoding.DecodeString(response)
	if !strings.Contains(string(raw), "EncryptedAssertion") || strings.Contains(string(raw), "ada@example.com") {
		t.Fatalf("the assertion was not encrypted:\n%s", raw)
	}
	sessionFrom(t, got)

	f.noEncrypt = true
	if rec, _, _ := f.signIn(t, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("an assertion in the clear = %d, want 401 when encryption is required", rec.Code)
	}
}

func TestSAMLKeyPairSettings(t *testing.T) {
	cert, key := spKeyPair(t)
	_, otherKey := spKeyPair(t)
	for name, tc := range map[string]struct{ extra, want string }{
		"half a pair":        {`sp_certificate: "` + cert + `"`, "set both"},
		"mismatched pair":    {`sp_certificate: "` + cert + `"` + "\n    sp_private_key: \"" + otherKey + `"`, "sp_private_key"},
		"encryption, no key": {`require_encrypted_assertions: true`, "require_encrypted_assertions needs"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse([]byte(`
store: {driver: sqlite, dsn: x}
sso:
  saml:
    idp_metadata: "<x/>"
    acs_url: "http://example.com/ui/auth/saml/acs"
    ` + tc.extra + `
  roles: [{group: g, role: owner}]
`))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := config.Parse([]byte(`
store: {driver: sqlite, dsn: x}
sso:
  saml:
    idp_metadata: "<x/>"
    acs_url: "http://example.com/ui/auth/saml/acs"
    sp_certificate: "` + cert + `"
    sp_private_key: "` + key + `"
  roles: [{group: g, role: owner}]
`)); err != nil {
		t.Fatal(err)
	}
}

// follow sends a redirect's target to h, with cookies, and returns the reply.
func follow(h http.Handler, location string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", location, nil)
	for _, c := range cookies {
		if c != nil {
			req.AddCookie(c)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cookieNamed(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestSAMLSingleLogoutFromTheGateway(t *testing.T) {
	f, e := newHardenedSAML(t)
	got, _, _ := f.signIn(t, "")
	session := sessionFrom(t, got)

	out := e.browser(session, "POST", "/ui/auth/logout", "")
	var body struct{ Redirect string }
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &body) != nil || !strings.HasPrefix(body.Redirect, f.srv.URL+"/slo?") {
		t.Fatalf("logout = %d %s", out.Code, out.Body)
	}
	if rec := e.browser(session, "GET", "/admin/v1/me", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the gateway session survived sign-out: %d", rec.Code)
	}

	// The identity provider verifies the signed LogoutRequest, ends its
	// session and answers the gateway.
	idp := follow(http.HandlerFunc(f.idp.ServeSLO), body.Redirect)
	if f.loggedOut != 1 || idp.Code != http.StatusFound {
		t.Fatalf("the identity provider's logout = %d (ended %d sessions): %s", idp.Code, f.loggedOut, idp.Body)
	}
	back := idp.Header().Get("Location")
	if !strings.HasPrefix(back, "http://example.com/ui/auth/saml/slo?SAMLResponse=") {
		t.Fatalf("the identity provider answered to %s", back)
	}
	done := follow(e.h, back, cookieNamed(out, samlCookie))
	if done.Code != http.StatusFound || done.Header().Get("Location") != "/ui/" {
		t.Fatalf("the logout response = %d %s", done.Code, done.Header().Get("Location"))
	}
}

func TestSAMLSingleLogoutFromTheIdentityProvider(t *testing.T) {
	f, e := newHardenedSAML(t)
	got, _, _ := f.signIn(t, "")
	session := sessionFrom(t, got)
	got2, _, _ := f.signIn(t, "")
	session2 := sessionFrom(t, got2)

	// The identity provider sends the browser to the gateway with a
	// signed LogoutRequest.
	start := httptest.NewRecorder()
	f.idp.StartLogout(start, httptest.NewRequest("GET", f.srv.URL+"/logout", nil), f.user, "")
	toGateway := start.Header().Get("Location")
	if start.Code != http.StatusFound || !strings.HasPrefix(toGateway, "http://example.com/ui/auth/saml/slo?SAMLRequest=") {
		t.Fatalf("start logout = %d %s %s", start.Code, toGateway, start.Body)
	}

	// Tampering with the signed request is refused, and ends nothing.
	if rec := follow(e.h, strings.Replace(toGateway, "SigAlg=", "SigAlg=x", 1)); rec.Code != http.StatusBadRequest {
		t.Errorf("a request with a changed SigAlg = %d, want 400", rec.Code)
	}
	if rec := follow(e.h, toGateway[:strings.Index(toGateway, "&Signature=")]); rec.Code != http.StatusBadRequest {
		t.Errorf("an unsigned request = %d, want 400", rec.Code)
	}
	if rec := e.browser(session, "GET", "/admin/v1/me", ""); rec.Code != 200 {
		t.Fatalf("a refused logout request ended the session: %d", rec.Code)
	}

	rec := follow(e.h, toGateway)
	toIdP := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.HasPrefix(toIdP, f.srv.URL+"/slo?SAMLResponse=") || !strings.Contains(toIdP, "&Signature=") {
		t.Fatalf("the gateway's logout response = %d %s %s", rec.Code, toIdP, rec.Body)
	}
	for _, s := range []string{session, session2} {
		if rec := e.browser(s, "GET", "/admin/v1/me", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("a session survived the identity provider's logout: %d", rec.Code)
		}
	}

	// The identity provider accepts the gateway's signed response.
	var idpCookies []*http.Cookie
	idpCookies = append(idpCookies, start.Result().Cookies()...)
	fin := follow(http.HandlerFunc(f.idp.ServeSLO), toIdP, idpCookies...)
	if fin.Code != http.StatusOK || !strings.Contains(strings.ToLower(fin.Body.String()), "logged out") {
		t.Fatalf("the identity provider did not accept the gateway's logout response: %d %s", fin.Code, fin.Body)
	}
}
