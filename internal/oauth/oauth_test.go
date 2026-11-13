package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// issuer is a fake authorization server publishing OIDC metadata and a
// JWKS. Keys can be rotated during a test.
type issuer struct {
	*httptest.Server
	mu       sync.Mutex
	rsa      map[string]*rsa.PrivateKey
	ec       map[string]*ecdsa.PrivateKey
	jwksHits atomic.Int32
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{rsa: map[string]*rsa.PrivateKey{}, ec: map[string]*ecdsa.PrivateKey{}}
	is.addRSA(t, "r1")
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	is.ec["e1"] = ek
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": is.URL + "/tenant", "jwks_uri": is.URL + "/keys"})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		is.jwksHits.Add(1)
		is.mu.Lock()
		defer is.mu.Unlock()
		b64 := base64.RawURLEncoding.EncodeToString
		var keys []map[string]string
		for kid, k := range is.rsa {
			keys = append(keys, map[string]string{"kty": "RSA", "kid": kid, "use": "sig", "n": b64(k.N.Bytes()), "e": b64(big.NewInt(int64(k.E)).Bytes())})
		}
		for kid, k := range is.ec {
			pub, _ := k.PublicKey.Bytes()
			keys = append(keys, map[string]string{"kty": "EC", "kid": kid, "crv": "P-256", "x": b64(pub[1:33]), "y": b64(pub[33:])})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	return is
}

func (is *issuer) addRSA(t *testing.T, kid string) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is.mu.Lock()
	is.rsa[kid] = k
	is.mu.Unlock()
}

func (is *issuer) sign(t *testing.T, method jwt.SigningMethod, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	tok.Header["kid"] = kid
	var key any
	is.mu.Lock()
	if k, ok := is.rsa[kid]; ok {
		key = k
	} else {
		key = is.ec[kid]
	}
	is.mu.Unlock()
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const resource = "https://gw.example.com/mcp/eng"

func (is *issuer) claims(mod func(jwt.MapClaims)) jwt.MapClaims {
	c := jwt.MapClaims{
		"iss": is.URL + "/tenant", "sub": "user-1", "aud": "https://gw.example.com/mcp/eng/",
		"exp": time.Now().Add(time.Hour).Unix(), "scope": "openid mcp",
	}
	if mod != nil {
		mod(c)
	}
	return c
}

func TestVerify(t *testing.T) {
	is := newIssuer(t)
	v := NewVerifier(Config{Issuer: is.URL + "/tenant", Scopes: []string{"mcp"}}, http.DefaultClient)
	ctx := context.Background()

	tok, err := v.Verify(ctx, is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(nil)), []string{resource})
	if err != nil || tok.Subject != "user-1" {
		t.Fatalf("valid RS256 token: %+v, %v", tok, err)
	}
	if _, err := v.Verify(ctx, is.sign(t, jwt.SigningMethodES256, "e1", is.claims(func(c jwt.MapClaims) {
		delete(c, "scope")
		c["scp"] = []any{"mcp"}
		c["aud"] = []any{"other", "api://gateway"}
	})), []string{resource, "api://gateway"}); err != nil {
		t.Fatalf("valid ES256 token with scp and a configured audience: %v", err)
	}

	for name, tc := range map[string]struct {
		token string
		want  error
	}{
		"wrong issuer":   {is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(func(c jwt.MapClaims) { c["iss"] = "https://evil" })), ErrInvalidToken},
		"expired":        {is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-2 * time.Minute).Unix() })), ErrInvalidToken},
		"no expiry":      {is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(func(c jwt.MapClaims) { delete(c, "exp") })), ErrInvalidToken},
		"not yet valid":  {is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() })), ErrInvalidToken},
		"other audience": {is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(func(c jwt.MapClaims) { c["aud"] = "https://gw.example.com/mcp/other" })), ErrInvalidToken},
		"no subject":     {is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(func(c jwt.MapClaims) { delete(c, "sub") })), ErrInvalidToken},
		"missing scope":  {is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(func(c jwt.MapClaims) { c["scope"] = "openid" })), ErrInsufficientScope},
		"unsigned":       {unsigned(t, is.claims(nil)), ErrInvalidToken},
		"hmac":           {hmacSigned(t, is.claims(nil)), ErrInvalidToken},
		"garbage":        {"not.a.token", ErrInvalidToken},
	} {
		if _, err := v.Verify(ctx, tc.token, []string{resource}); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestKeyRotation(t *testing.T) {
	is := newIssuer(t)
	v := NewVerifier(Config{Issuer: is.URL + "/tenant"}, http.DefaultClient)
	ctx := context.Background()
	if _, err := v.Verify(ctx, is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(nil)), []string{resource}); err != nil {
		t.Fatal(err)
	}
	is.addRSA(t, "r2")
	// A token signed with a new key triggers one fetch, rate limited.
	v.lastAttempt = time.Time{}
	if _, err := v.Verify(ctx, is.sign(t, jwt.SigningMethodRS256, "r2", is.claims(nil)), []string{resource}); err != nil {
		t.Fatalf("token signed with a rotated-in key: %v", err)
	}
	hits := is.jwksHits.Load()
	for range 5 {
		_, _ = v.Verify(ctx, is.sign(t, jwt.SigningMethodRS256, "r1", is.claims(func(c jwt.MapClaims) {})), []string{resource})
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, is.claims(nil))
		tok.Header["kid"] = "made-up"
		s, _ := tok.SignedString(is.rsa["r1"])
		if _, err := v.Verify(ctx, s, []string{resource}); err == nil {
			t.Fatal("a token with an unknown key id was accepted")
		}
	}
	if is.jwksHits.Load() != hits {
		t.Fatalf("unknown key ids caused %d extra JWKS fetches", is.jwksHits.Load()-hits)
	}
}

func unsigned(t *testing.T, c jwt.MapClaims) string {
	s, err := jwt.NewWithClaims(jwt.SigningMethodNone, c).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func hmacSigned(t *testing.T, c jwt.MapClaims) string {
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
