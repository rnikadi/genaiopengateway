package server

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/beevik/etree"
	saml2 "github.com/russellhaering/gosaml2"
	samltypes "github.com/russellhaering/gosaml2/types"

	"github.com/rnikadi/genaiopengateway/internal/store"
)

// SAML single logout (docs/saml.md). With a key pair, signing out of the
// gateway also sends the browser to the identity provider with a signed
// LogoutRequest, and the identity provider's own LogoutRequests end the
// user's gateway sessions on every replica. Both directions use the
// redirect binding out; in, the gateway takes the redirect and POST
// bindings.

// samlRedirectSigAlgs are the signature algorithms the redirect binding
// may use. SHA-1 is not among them.
var samlRedirectSigAlgs = map[string]x509.SignatureAlgorithm{
	"http://www.w3.org/2001/04/xmldsig-more#rsa-sha256":   x509.SHA256WithRSA,
	"http://www.w3.org/2001/04/xmldsig-more#rsa-sha512":   x509.SHA512WithRSA,
	"http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha256": x509.ECDSAWithSHA256,
	"http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha512": x509.ECDSAWithSHA512,
}

// samlMessageLimit bounds an inflated logout message.
const samlMessageLimit = 1 << 20

// samlLogoutClockSkew is how far a logout request's IssueInstant may be
// from now.
const samlLogoutClockSkew = 5 * time.Minute

// samlLogoutURL returns where to send the browser so that the identity
// provider ends its session too, or "" when single logout does not apply
// to this session.
func (s *Server) samlLogoutURL(w http.ResponseWriter, r *http.Request, ss *store.Session) string {
	cfg := s.rt.Load().cfg.SSO.SAML
	if cfg == nil || ss.Claims == nil || ss.Claims.SAML == nil || ss.Claims.SAML.NameID == "" {
		return ""
	}
	p, err := s.samlSP(r.Context(), cfg)
	if err != nil {
		s.log.Warn("single logout skipped: the identity provider cannot be read", "err", err)
		return ""
	}
	if !p.singleLogout() {
		return ""
	}
	n := ss.Claims.SAML
	doc, err := p.sp.BuildLogoutRequestDocumentNoSig(n.NameID, n.SessionIndex)
	if err != nil {
		s.log.Warn("building a SAML logout request failed", "err", err)
		return ""
	}
	// The NameID goes back exactly as the identity provider sent it.
	root := doc.Root()
	for _, el := range root.ChildElements() {
		switch el.Tag {
		case "NameID":
			setOrRemoveAttr(el, "Format", n.Format)
			setOrRemoveAttr(el, "NameQualifier", n.NameQualifier)
			setOrRemoveAttr(el, "SPNameQualifier", n.SPNameQualifier)
		case "SessionIndex":
			if n.SessionIndex == "" {
				root.RemoveChild(el)
			}
		}
	}
	to, err := samlRedirectURL(p, p.sp.IdentityProviderSLOURL, "SAMLRequest", doc, "")
	if err != nil {
		s.log.Warn("signing a SAML logout request failed", "err", err)
		return ""
	}
	raw, _ := json.Marshal(samlFlow{RequestID: root.SelectAttrValue("ID", ""), Return: "/ui/"})
	setSAMLCookie(w, r, base64.RawURLEncoding.EncodeToString(raw), authFlowTTL)
	return to
}

func setOrRemoveAttr(el *etree.Element, key, value string) {
	if value == "" {
		el.RemoveAttr(key)
		return
	}
	el.CreateAttr(key, value)
}

// samlRedirectURL encodes a message for the redirect binding and signs
// it (saml-bindings-2.0-os §3.4.4.1).
func samlRedirectURL(p *samlProvider, dest, param string, doc *etree.Document, relayState string) (string, error) {
	msg, err := doc.WriteToBytes()
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(msg); err != nil {
		return "", err
	}
	if err := fw.Close(); err != nil {
		return "", err
	}
	q := param + "=" + url.QueryEscape(base64.StdEncoding.EncodeToString(buf.Bytes()))
	if relayState != "" {
		q += "&RelayState=" + url.QueryEscape(relayState)
	}
	ctx := p.sp.SigningContext()
	if ctx == nil {
		return "", errors.New("the gateway has no signing key")
	}
	q += "&SigAlg=" + url.QueryEscape(ctx.GetSignatureMethodIdentifier())
	sig, err := ctx.SignString(q)
	if err != nil {
		return "", err
	}
	q += "&Signature=" + url.QueryEscape(base64.StdEncoding.EncodeToString(sig))
	u, err := url.Parse(dest)
	if err != nil {
		return "", err
	}
	if u.RawQuery != "" {
		q = u.RawQuery + "&" + q
	}
	u.RawQuery = q
	return u.String(), nil
}

// samlRedirectMessage verifies a redirect-binding message's signature
// against the identity provider's certificates and returns its XML. The
// signature covers the parameters exactly as they appear in the URL.
func samlRedirectMessage(p *samlProvider, rawQuery, param string) ([]byte, string, error) {
	raw := map[string]string{}
	for _, part := range strings.Split(rawQuery, "&") {
		k, v, _ := strings.Cut(part, "=")
		if _, dup := raw[k]; dup {
			return nil, "", fmt.Errorf("the parameter %s appears twice", k)
		}
		raw[k] = v
	}
	if raw[param] == "" || raw["SigAlg"] == "" || raw["Signature"] == "" {
		return nil, "", errors.New("the message is not signed")
	}
	sigAlg, err := url.QueryUnescape(raw["SigAlg"])
	if err != nil {
		return nil, "", err
	}
	alg, ok := samlRedirectSigAlgs[sigAlg]
	if !ok {
		return nil, "", fmt.Errorf("the signature algorithm %q is not accepted", sigAlg)
	}
	signed := param + "=" + raw[param]
	if rs, ok := raw["RelayState"]; ok {
		signed += "&RelayState=" + rs
	}
	signed += "&SigAlg=" + raw["SigAlg"]
	sigText, err := url.QueryUnescape(raw["Signature"])
	if err != nil {
		return nil, "", err
	}
	sig, err := base64.StdEncoding.DecodeString(sigText)
	if err != nil {
		return nil, "", fmt.Errorf("the signature is not base64: %w", err)
	}
	verified := false
	for _, cert := range p.idpCerts {
		if cert.CheckSignature(alg, []byte(signed), sig) == nil {
			verified = true
			break
		}
	}
	if !verified {
		return nil, "", errors.New("the signature is not the identity provider's")
	}
	text, err := url.QueryUnescape(raw[param])
	if err != nil {
		return nil, "", err
	}
	deflated, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, "", fmt.Errorf("the message is not base64: %w", err)
	}
	msg, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(deflated)), samlMessageLimit+1))
	if err != nil {
		return nil, "", fmt.Errorf("the message cannot be inflated: %w", err)
	}
	if len(msg) > samlMessageLimit {
		return nil, "", errors.New("the message is too large")
	}
	relay, _ := url.QueryUnescape(raw["RelayState"])
	return msg, relay, nil
}

// samlSLO is the gateway's single logout service: it takes the identity
// provider's LogoutRequests and its LogoutResponses to the gateway's own.
func (s *Server) samlSLO(w http.ResponseWriter, r *http.Request) {
	cfg := s.rt.Load().cfg.SSO.SAML
	if cfg == nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.samlSP(r.Context(), cfg)
	if err != nil {
		s.log.Error("reading the SAML identity provider failed", "err", err)
		http.Error(w, "The identity provider cannot be reached. Try again shortly.", http.StatusBadGateway)
		return
	}
	if !p.singleLogout() {
		http.Error(w, "Single logout needs the gateway's key pair and an identity provider with a logout endpoint.", http.StatusNotFound)
		return
	}
	var request, response string
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxAdminBody)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "The logout message cannot be read.", http.StatusBadRequest)
			return
		}
		request, response = r.PostForm.Get("SAMLRequest"), r.PostForm.Get("SAMLResponse")
	} else {
		q := r.URL.Query()
		request, response = q.Get("SAMLRequest"), q.Get("SAMLResponse")
	}
	switch {
	case request != "":
		s.samlLogoutRequest(w, r, p, request)
	case response != "":
		s.samlLogoutResponse(w, r, p, response)
	default:
		http.Error(w, "No logout message.", http.StatusBadRequest)
	}
}

// samlLogoutRequest ends the sessions the identity provider names, and
// answers with a signed LogoutResponse.
func (s *Server) samlLogoutRequest(w http.ResponseWriter, r *http.Request, p *samlProvider, encoded string) {
	fail := func(err error) {
		s.log.Warn("a SAML logout request was refused", "err", err)
		http.Error(w, "The identity provider's logout request is not valid.", http.StatusBadRequest)
	}
	var req *saml2.LogoutRequest
	var relay string
	if r.Method == http.MethodPost {
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			fail(err)
			return
		}
		if samlUsesSHA1(raw) {
			fail(errors.New("the request is signed with SHA-1"))
			return
		}
		if req, err = p.sp.ValidateEncodedLogoutRequestPOST(encoded); err != nil {
			fail(err)
			return
		}
		relay = r.PostForm.Get("RelayState")
	} else {
		msg, rs, err := samlRedirectMessage(p, r.URL.RawQuery, "SAMLRequest")
		if err != nil {
			fail(err)
			return
		}
		req = &saml2.LogoutRequest{}
		if err := xml.Unmarshal(msg, req); err != nil {
			fail(err)
			return
		}
		if err := p.sp.ValidateDecodedLogoutRequest(req); err != nil {
			fail(err)
			return
		}
		relay = rs
	}
	if d := time.Since(req.IssueInstant); d > samlLogoutClockSkew || d < -samlLogoutClockSkew {
		fail(fmt.Errorf("the request was issued at %s, which is not now", req.IssueInstant))
		return
	}
	if req.NameID == nil || strings.TrimSpace(req.NameID.Value) == "" {
		fail(errors.New("the request names nobody"))
		return
	}
	subject := strings.TrimSpace(req.NameID.Value)
	status := saml2.StatusCodeSuccess
	n, err := s.endSAMLSessions(r.Context(), subject)
	if err != nil {
		s.log.Error("ending sessions for a SAML logout failed", "err", err)
		status = "urn:oasis:names:tc:SAML:2.0:status:Responder"
	} else {
		s.log.Info("single logout from the identity provider", "subject", subject, "sessions", n)
	}
	setCookie(w, r, sessionCookie, "", -1)
	doc, err := p.sp.BuildLogoutResponseDocumentNoSig(status, req.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	to, err := samlRedirectURL(p, p.sp.IdentityProviderSLOURL, "SAMLResponse", doc, relay)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, to, http.StatusFound)
}

// endSAMLSessions signs a user out of every session, on every replica.
func (s *Server) endSAMLSessions(ctx context.Context, subject string) (int64, error) {
	n, err := s.store.DeleteOtherSessions(ctx, "", subject)
	if err != nil {
		return 0, err
	}
	details, _ := json.Marshal(map[string]any{"sessions": n, "by": "identity provider"})
	return n, s.store.AppendAudit(ctx, &store.AuditEntry{
		Actor: subject, Action: "operator.sign_out", ResourceType: "operator", ResourceID: subject, Details: details,
	})
}

// samlLogoutResponse finishes a sign-out the gateway started. The
// gateway's session already ended, so a failure here is only logged.
func (s *Server) samlLogoutResponse(w http.ResponseWriter, r *http.Request, p *samlProvider, encoded string) {
	var f samlFlow
	raw, _ := base64.RawURLEncoding.DecodeString(readCookie(r, samlCookie))
	_ = json.Unmarshal(raw, &f)
	setSAMLCookie(w, r, "", -1)
	err := func() error {
		var resp *samltypes.LogoutResponse
		if r.Method == http.MethodPost {
			raw, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return err
			}
			if samlUsesSHA1(raw) {
				return errors.New("the response is signed with SHA-1")
			}
			if resp, err = p.sp.ValidateEncodedLogoutResponsePOST(encoded); err != nil {
				return err
			}
		} else {
			msg, _, err := samlRedirectMessage(p, r.URL.RawQuery, "SAMLResponse")
			if err != nil {
				return err
			}
			resp = &samltypes.LogoutResponse{}
			if err := xml.Unmarshal(msg, resp); err != nil {
				return err
			}
			if err := p.sp.ValidateDecodedLogoutResponse(resp); err != nil {
				return err
			}
		}
		if f.RequestID == "" || resp.InResponseTo != f.RequestID {
			return fmt.Errorf("the response answers %q, not this browser's request", resp.InResponseTo)
		}
		return nil
	}()
	if err != nil {
		s.log.Warn("the identity provider's logout response was not accepted; the gateway session had already ended", "err", err)
	}
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

// samlUsesSHA1 reports whether an XML message has a signature that uses
// a SHA-1 algorithm. Only signatures count: RSA-OAEP key transport in an
// encrypted assertion names a SHA-1 digest too, and that is safe.
func samlUsesSHA1(raw []byte) bool {
	doc := etree.NewDocument()
	if err := doc.ReadFromBytes(raw); err != nil || doc.Root() == nil {
		return false // the validation that follows refuses it
	}
	var walk func(el *etree.Element, inSig bool) bool
	walk = func(el *etree.Element, inSig bool) bool {
		inSig = inSig || (el.Tag == "Signature" && el.NamespaceURI() == dsigNamespace)
		if inSig && (el.Tag == "SignatureMethod" || el.Tag == "DigestMethod") && slices.Contains(samlSHA1, el.SelectAttrValue("Algorithm", "")) {
			return true
		}
		for _, c := range el.ChildElements() {
			if walk(c, inSig) {
				return true
			}
		}
		return false
	}
	return walk(doc.Root(), false)
}

const dsigNamespace = "http://www.w3.org/2000/09/xmldsig#"
