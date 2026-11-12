package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/beevik/etree"
	saml2 "github.com/russellhaering/gosaml2"
	samltypes "github.com/russellhaering/gosaml2/types"
	dsig "github.com/russellhaering/goxmldsig"
	dsigtypes "github.com/russellhaering/goxmldsig/types"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Operator sign-in with a SAML 2.0 identity provider (docs/saml.md). The
// gateway is the service provider: it sends the browser to the identity
// provider with an AuthnRequest over the redirect binding and takes the
// signed response on its assertion consumer service over the POST
// binding. Roles and sessions then work as with OIDC.
//
// With a key pair of its own (sp_certificate and sp_private_key) the
// gateway also signs its AuthnRequests, decrypts encrypted assertions
// and takes part in single logout (saml_logout.go).

const samlCookie = "gog_saml"

// samlProvider is the service provider for the current settings, with
// the identity provider's metadata.
type samlProvider struct {
	cfg config.SAML
	sp  *saml2.SAMLServiceProvider
	// key is the gateway's own key pair, or nil.
	key *tls.Certificate
	// idpCerts verify the identity provider's redirect-binding signatures.
	idpCerts []*x509.Certificate
}

// singleLogout reports whether sign-out is passed on to the identity
// provider: that needs the gateway's key, to sign the request, and an
// identity provider that takes logout requests over the redirect binding.
func (p *samlProvider) singleLogout() bool {
	return p.key != nil && p.sp.IdentityProviderSLOURL != ""
}

// samlEntities is metadata that lists several entities.
type samlEntities struct {
	XMLName  xml.Name                     `xml:"urn:oasis:names:tc:SAML:2.0:metadata EntitiesDescriptor"`
	Entities []samltypes.EntityDescriptor `xml:"EntityDescriptor"`
}

// parseIdPMetadata reads an identity provider's metadata: an entity, or
// the first entity of a list that is an identity provider.
func parseIdPMetadata(raw []byte) (*samltypes.EntityDescriptor, error) {
	var ed samltypes.EntityDescriptor
	if err := xml.Unmarshal(raw, &ed); err == nil && ed.IDPSSODescriptor != nil {
		return &ed, nil
	}
	var list samlEntities
	if err := xml.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("not SAML metadata: %w", err)
	}
	for i := range list.Entities {
		if list.Entities[i].IDPSSODescriptor != nil {
			return &list.Entities[i], nil
		}
	}
	return nil, errors.New("the metadata has no identity provider")
}

// samlSP builds the service provider, fetching the identity provider's
// metadata on first use and again when the settings change.
func (s *Server) samlSP(ctx context.Context, cfg *config.SAML) (*samlProvider, error) {
	s.ssoMu.Lock()
	defer s.ssoMu.Unlock()
	if p := s.saml; p != nil && reflect.DeepEqual(p.cfg, *cfg) {
		return p, nil
	}
	raw := []byte(cfg.IDPMetadata)
	if cfg.IDPMetadata == "" {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.IDPMetadataURL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("reading the identity provider's metadata: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("reading the identity provider's metadata: %s", resp.Status)
		}
		if raw, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20)); err != nil {
			return nil, fmt.Errorf("reading the identity provider's metadata: %w", err)
		}
	}
	md, err := parseIdPMetadata(raw)
	if err != nil {
		return nil, fmt.Errorf("reading the identity provider's metadata: %w", err)
	}
	certs := &dsig.MemoryX509CertificateStore{}
	for _, kd := range md.IDPSSODescriptor.KeyDescriptors {
		if kd.Use != "" && kd.Use != "signing" {
			continue
		}
		for _, xc := range kd.KeyInfo.X509Data.X509Certificates {
			der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(xc.Data), ""))
			if err != nil {
				return nil, fmt.Errorf("the identity provider's certificate: %w", err)
			}
			cert, err := x509.ParseCertificate(der)
			if err != nil {
				return nil, fmt.Errorf("the identity provider's certificate: %w", err)
			}
			certs.Roots = append(certs.Roots, cert)
		}
	}
	if len(certs.Roots) == 0 {
		return nil, errors.New("the identity provider's metadata has no signing certificate")
	}
	p := &samlProvider{cfg: *cfg}
	var ssoURL string
	for _, sso := range md.IDPSSODescriptor.SingleSignOnServices {
		if sso.Binding == saml2.BindingHttpRedirect {
			ssoURL = sso.Location
			break
		}
	}
	if ssoURL == "" {
		return nil, errors.New("the identity provider has no sign-in endpoint for the redirect binding")
	}
	p.sp = &saml2.SAMLServiceProvider{
		IdentityProviderSSOURL: ssoURL, IdentityProviderIssuer: md.EntityID,
		AssertionConsumerServiceURL: cfg.ACSURL, ServiceProviderIssuer: cfg.EntityID, AudienceURI: cfg.EntityID,
		IDPCertificateStore: certs, NameIdFormat: "urn:oasis:names:tc:SAML:1.1:nameid-format:unspecified",
		AllowMissingAttributes: true,
		Clock:                  dsig.NewRealClock(),
	}
	p.idpCerts = certs.Roots
	pair, err := cfg.SPKeyPair()
	if err != nil {
		return nil, err
	}
	if pair != nil {
		p.key = pair
		// The key decrypts assertions and signs requests.
		p.sp.SPKeyStore = dsig.TLSCertKeyStore(*pair)
		p.sp.SignAuthnRequests = true
		p.sp.SignAuthnRequestsAlgorithm = dsig.RSASHA256SignatureMethod
		p.sp.ServiceProviderSLOURL = samlSLOURL(cfg)
		for _, slo := range md.IDPSSODescriptor.SingleLogoutServices {
			if slo.Binding == saml2.BindingHttpRedirect {
				p.sp.IdentityProviderSLOURL = slo.Location
				break
			}
		}
	}
	s.saml = p
	return p, nil
}

// samlReplay refuses an assertion used before, on any replica.
func (s *Server) samlReplay(ctx context.Context, assertionID string, expiresAt time.Time) error {
	return s.store.ConsumeAssertion(ctx, assertionID, expiresAt)
}

// samlFlow is what the sign-in remembers between the redirect and the
// response: the request id the response must answer, and where to go
// afterwards.
type samlFlow struct {
	RequestID string `json:"i"`
	Return    string `json:"r"`
	// Connect and Mine mark a sign-in that only proves who the user is,
	// for the MCP connect flow and connections page (see authFlow). The
	// cookie is not signed and need not be: whatever it says, the
	// assertion's subject must equal the connect request's.
	Connect string `json:"c,omitempty"`
	Mine    bool   `json:"m,omitempty"`
}

// samlCookieOptions lets the identity provider's cross-site POST carry
// the flow cookie: SameSite=None needs Secure, so it applies over HTTPS.
func setSAMLCookie(w http.ResponseWriter, r *http.Request, value string, maxAge time.Duration) {
	c := &http.Cookie{
		Name: cookieName(r, samlCookie), Value: value, Path: "/", MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode,
	}
	if secure(r) {
		c.SameSite = http.SameSiteNoneMode
	}
	if maxAge < 0 {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// samlMetadata serves the gateway's SAML service provider metadata for the identity provider.
func (s *Server) samlMetadata(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg.SSO.SAML
	if cfg == nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.samlSP(r.Context(), cfg)
	if err != nil {
		s.log.Error("reading the SAML identity provider failed", "err", err)
		http.Error(w, "The identity provider's metadata cannot be read.", http.StatusBadGateway)
		return
	}
	md := &samltypes.EntityDescriptor{
		ValidUntil: time.Now().UTC().Add(7 * 24 * time.Hour),
		EntityID:   p.sp.ServiceProviderIssuer,
		SPSSODescriptor: &samltypes.SPSSODescriptor{
			WantAssertionsSigned:       true,
			ProtocolSupportEnumeration: saml2.SAMLProtocolNamespace,
			NameIDFormats:              []string{p.sp.NameIdFormat},
			AssertionConsumerServices: []samltypes.IndexedEndpoint{{
				Binding: saml2.BindingHttpPost, Location: p.sp.AssertionConsumerServiceURL, Index: 1,
			}},
		},
	}
	// Without a key pair the gateway signs and decrypts nothing, so it
	// lists no keys and no logout service.
	if p.key != nil {
		cert := []dsigtypes.X509Certificate{{Data: base64.StdEncoding.EncodeToString(p.key.Certificate[0])}}
		d := md.SPSSODescriptor
		d.AuthnRequestsSigned = true
		d.KeyDescriptors = []samltypes.KeyDescriptor{
			{Use: "signing", KeyInfo: dsigtypes.KeyInfo{X509Data: dsigtypes.X509Data{X509Certificates: cert}}},
			{Use: "encryption", KeyInfo: dsigtypes.KeyInfo{X509Data: dsigtypes.X509Data{X509Certificates: cert}},
				// AES-GCM first: CBC has no integrity protection.
				EncryptionMethods: []samltypes.EncryptionMethod{
					{Algorithm: samltypes.MethodAES256GCM}, {Algorithm: samltypes.MethodAES128GCM},
					{Algorithm: samltypes.MethodAES256CBC}, {Algorithm: samltypes.MethodAES128CBC},
				}},
		}
		d.SingleLogoutServices = []samltypes.Endpoint{
			{Binding: saml2.BindingHttpRedirect, Location: p.sp.ServiceProviderSLOURL},
			{Binding: saml2.BindingHttpPost, Location: p.sp.ServiceProviderSLOURL},
		}
	}
	raw, err := xml.MarshalIndent(md, "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/samlmetadata+xml")
	_, _ = w.Write(append([]byte(xml.Header), raw...))
}

// samlLogin sends the browser to the SAML identity provider with an AuthnRequest.
func (s *Server) samlLogin(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg.SSO.SAML
	if cfg == nil {
		http.NotFound(w, r)
		return
	}
	s.samlStart(w, r, cfg, samlFlow{Return: returnPath(r.URL.Query().Get("return"))})
}

// samlStart sends the browser to the identity provider and remembers f
// for the response.
func (s *Server) samlStart(w http.ResponseWriter, r *http.Request, cfg *config.SAML, f samlFlow) {
	p, err := s.samlSP(r.Context(), cfg)
	if err != nil {
		s.log.Error("reading the SAML identity provider failed", "err", err)
		http.Error(w, "The identity provider cannot be reached. Try again shortly.", http.StatusBadGateway)
		return
	}
	doc, err := p.sp.BuildAuthRequestDocumentNoSig()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	to, err := p.sp.BuildAuthURLRedirect("", doc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f.RequestID = doc.Root().SelectAttrValue("ID", "")
	raw, _ := json.Marshal(f)
	setSAMLCookie(w, r, base64.RawURLEncoding.EncodeToString(raw), authFlowTTL)
	http.Redirect(w, r, to, http.StatusFound)
}

// samlACS takes the identity provider's signed response, resolves roles and starts a session.
func (s *Server) samlACS(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg
	mc := cfg.SSO.SAML
	if mc == nil {
		http.NotFound(w, r)
		return
	}
	fail := func(status int, msg string, err error) {
		s.log.Warn("operator sign-in failed", "reason", msg, "err", err)
		http.Error(w, msg, status)
	}
	var f samlFlow
	raw, err := base64.RawURLEncoding.DecodeString(readCookie(r, samlCookie))
	setSAMLCookie(w, r, "", -1)
	if err != nil || json.Unmarshal(raw, &f) != nil || f.RequestID == "" {
		fail(http.StatusBadRequest, "The sign-in expired or was started elsewhere. Start again from the gateway.", err)
		return
	}
	p, err := s.samlSP(r.Context(), mc)
	if err != nil {
		fail(http.StatusBadGateway, "The identity provider cannot be reached. Try again shortly.", err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)
	if err := r.ParseForm(); err != nil {
		fail(http.StatusBadRequest, "The identity provider's response cannot be read.", err)
		return
	}
	assertion, err := s.samlAssertion(r.Context(), p, r.PostForm.Get("SAMLResponse"), f.RequestID)
	if err != nil {
		fail(http.StatusUnauthorized, "The identity provider's response is not valid.", err)
		return
	}
	o := samlOperator(mc, assertion)
	if f.Connect != "" || f.Mine {
		// The user only proves who they are: no role and no session.
		subject := samlMCPSubject(mc, assertion)
		if verifySubject(cfg) != viaSAML || subject == "" {
			fail(http.StatusForbidden, "The identity provider's response does not say who you are to MCP servers. Ask an administrator to check the MCP subject attribute.", nil)
			return
		}
		if f.Mine {
			s.mySignedIn(w, r, cfg, subject, o.claims.Email)
		} else {
			s.connectSignedIn(w, r, f.Connect, subject)
		}
		return
	}
	o.claims.Groups = samlAttribute(assertion, mc.GroupsAttribute)
	groups, active, err := s.directoryGroups(r.Context(), o.claims)
	if err != nil {
		fail(http.StatusServiceUnavailable, "The directory of provisioned users cannot be read. Try again shortly.", err)
		return
	}
	if !active {
		fail(http.StatusForbidden, fmt.Sprintf("%s was deactivated in the directory.", o.actor()), nil)
		return
	}
	o.Grants = s.resolveGrants(cfg.SSO.Roles, o.claims.Email, groups)
	if len(o.Grants) == 0 {
		fail(http.StatusForbidden, fmt.Sprintf("%s has no role on this gateway. Ask an owner to map one of your groups to a role.", o.actor()), nil)
		return
	}
	if err := s.startSession(w, r, o); err != nil {
		fail(http.StatusInternalServerError, "The session could not be saved.", err)
		return
	}
	http.Redirect(w, r, f.Return, http.StatusFound)
}

// samlSHA1 names the SHA-1 signature and digest algorithms, which a
// response may not use: SHA-1 collisions are practical.
var samlSHA1 = []string{
	"http://www.w3.org/2000/09/xmldsig#rsa-sha1",
	"http://www.w3.org/2000/09/xmldsig#dsa-sha1",
	"http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha1",
	"http://www.w3.org/2000/09/xmldsig#hmac-sha1",
	"http://www.w3.org/2000/09/xmldsig#sha1",
}

// samlAssertion verifies a response and returns its one assertion. The
// signature (of the response or the assertion) must be the identity
// provider's; the response must answer requestID and be addressed to
// this gateway; the assertion must be current, for this gateway, and
// not used before.
func (s *Server) samlAssertion(ctx context.Context, p *samlProvider, encoded, requestID string) (*samltypes.Assertion, error) {
	sp := p.sp
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("the response is not base64: %w", err)
	}
	if samlUsesSHA1(raw) {
		return nil, errors.New("the response is signed with SHA-1")
	}
	if p.cfg.RequireEncryptedAssertions {
		if err := samlOnlyEncrypted(raw); err != nil {
			return nil, err
		}
	}
	resp, err := sp.ValidateEncodedResponse(encoded)
	if err != nil {
		return nil, err
	}
	if resp.InResponseTo != requestID {
		return nil, fmt.Errorf("the response answers request %q, not %q", resp.InResponseTo, requestID)
	}
	if len(resp.Assertions) != 1 {
		return nil, fmt.Errorf("the response has %d signed assertions, not one", len(resp.Assertions))
	}
	a := &resp.Assertions[0]
	if !resp.SignatureValidated && !a.SignatureValidated {
		return nil, errors.New("neither the response nor its assertion is signed")
	}
	// An encrypted assertion's signature was out of sight until now.
	if a.Signature != nil {
		for _, alg := range samlSHA1 {
			if bytes.Contains(a.Signature.SignatureDocument, []byte(alg)) {
				return nil, errors.New("the assertion is signed with SHA-1")
			}
		}
	}
	if a.Subject == nil || a.Subject.NameID == nil {
		return nil, errors.New("the assertion has no subject")
	}
	if sc := a.Subject.SubjectConfirmation; sc != nil && sc.SubjectConfirmationData != nil {
		if irt := sc.SubjectConfirmationData.InResponseTo; irt != "" && irt != requestID {
			return nil, fmt.Errorf("the assertion answers request %q, not %q", irt, requestID)
		}
	}
	warn, err := sp.VerifyAssertionConditions(a)
	if err != nil {
		return nil, err
	}
	if warn.InvalidTime {
		return nil, errors.New("the assertion is not valid now")
	}
	if warn.NotInAudience {
		return nil, errors.New("the assertion is for another service provider")
	}
	expires, err := time.Parse(time.RFC3339, a.Conditions.NotOnOrAfter)
	if err != nil {
		return nil, err
	}
	if a.ID == "" {
		return nil, errors.New("the assertion has no id")
	}
	if err := s.samlReplay(ctx, a.ID, expires); err != nil {
		if errors.Is(err, store.ErrReplayed) {
			return nil, errors.New("the assertion was used before")
		}
		return nil, err
	}
	return a, nil
}

// samlOperator reads who signed in from an assertion: the NameID is the
// subject, and the email is the email attribute or, without one, a NameID
// that looks like an address.
func samlOperator(cfg *config.SAML, a *samltypes.Assertion) *operator {
	o := &operator{claims: &store.SessionClaims{}}
	o.Subject = strings.TrimSpace(a.Subject.NameID.Value)
	n := a.Subject.NameID
	o.claims.SAML = &store.SAMLSession{NameID: n.Value, Format: n.Format, NameQualifier: n.NameQualifier, SPNameQualifier: n.SPNameQualifier}
	if a.AuthnStatement != nil {
		o.claims.SAML.SessionIndex = a.AuthnStatement.SessionIndex
	}
	email := ""
	if cfg.EmailAttribute != "" {
		if v := samlAttribute(a, cfg.EmailAttribute); len(v) > 0 {
			email = v[0]
		}
	} else {
		for _, name := range []string{"email", "mail", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress"} {
			if v := samlAttribute(a, name); len(v) > 0 {
				email = v[0]
				break
			}
		}
		if email == "" && strings.Contains(o.Subject, "@") {
			email = o.Subject
		}
	}
	o.Email, o.claims.Email = email, email
	names := []string{cfg.NameAttribute}
	if cfg.NameAttribute == "" {
		names = []string{"displayName", "name", "http://schemas.microsoft.com/identity/claims/displayname"}
	}
	for _, name := range names {
		if v := samlAttribute(a, name); len(v) > 0 {
			o.Name = v[0]
			break
		}
	}
	if o.Subject == "" {
		o.Subject = email
	}
	return o
}

// samlAttribute returns an attribute's values, matched by name or
// friendly name.
// samlMCPSubject returns the value that equals the subject of the
// user's MCP access tokens, or "" when the assertion does not carry
// exactly one.
func samlMCPSubject(cfg *config.SAML, a *samltypes.Assertion) string {
	if strings.EqualFold(cfg.MCPSubjectAttribute, "NameID") {
		return strings.TrimSpace(a.Subject.NameID.Value)
	}
	if v := samlAttribute(a, cfg.MCPSubjectAttribute); len(v) == 1 {
		return v[0]
	}
	return ""
}

func samlAttribute(a *samltypes.Assertion, name string) []string {
	if a.AttributeStatement == nil {
		return nil
	}
	var out []string
	for _, attr := range a.AttributeStatement.Attributes {
		if attr.Name != name && attr.FriendlyName != name {
			continue
		}
		for _, v := range attr.Values {
			if v := strings.TrimSpace(v.Value); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// samlSLOURL is the gateway's single logout service, next to its ACS.
func samlSLOURL(cfg *config.SAML) string {
	return strings.TrimSuffix(cfg.ACSURL, "/acs") + "/slo"
}

// samlOnlyEncrypted refuses a response that carries an assertion in the
// clear, when the settings require encryption.
func samlOnlyEncrypted(raw []byte) error {
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(raw); err != nil || doc.Root() == nil {
		return fmt.Errorf("the response is not XML: %v", err)
	}
	encrypted := 0
	for _, el := range doc.Root().ChildElements() {
		switch {
		case el.Tag == "Assertion" && el.NamespaceURI() == saml2.SAMLAssertionNamespace:
			return errors.New("the assertion is not encrypted, and the settings require encryption")
		case el.Tag == "EncryptedAssertion" && el.NamespaceURI() == saml2.SAMLAssertionNamespace:
			encrypted++
		}
	}
	if encrypted == 0 {
		return errors.New("the response has no encrypted assertion")
	}
	return nil
}
