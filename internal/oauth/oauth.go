// Package oauth validates OAuth 2.1 access tokens for the MCP gateway,
// which acts as a resource server (ADR 0014). Tokens are JWTs signed by
// one configured issuer, whose keys are fetched from its JWKS.
package oauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// Errors returned by Verify. A request with ErrInvalidToken gets 401; one
// with ErrInsufficientScope gets 403.
var (
	ErrInvalidToken      = errors.New("invalid token")
	ErrInsufficientScope = errors.New("insufficient scope")
)

const (
	// keyRefresh is how often keys are fetched again, to pick up rotation.
	keyRefresh = time.Hour
	// minFetchInterval bounds fetches triggered by unknown key ids, so
	// tokens with made-up key ids cannot flood the issuer.
	minFetchInterval = 30 * time.Second
	leeway           = time.Minute
)

// Config is what a Verifier checks tokens against.
type Config struct {
	Issuer string
	// JWKSURL overrides discovery from the issuer's metadata.
	JWKSURL string
	Scopes  []string
}

// Verifier validates access tokens. It is safe for concurrent use.
type Verifier struct {
	cfg    Config
	client *http.Client

	mu          sync.Mutex
	jwksURL     string
	keys        map[string]crypto.PublicKey
	fetched     time.Time
	lastAttempt time.Time
}

// NewVerifier returns a verifier. Keys are fetched on first use.
func NewVerifier(cfg Config, client *http.Client) *Verifier {
	return &Verifier{cfg: cfg, client: client, jwksURL: cfg.JWKSURL}
}

// Token is what the gateway uses from a valid token.
type Token struct {
	Subject string
	Scopes  []string
	Claims  jwt.MapClaims
}

// Verify checks a token's signature, issuer, lifetime, audience and
// scopes. The audience must name one of resources.
func (v *Verifier) Verify(ctx context.Context, raw string, resources []string) (*Token, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(leeway),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	aud, _ := claims.GetAudience()
	if !slices.ContainsFunc(resources, func(r string) bool { return oauthex.MatchesResource(aud, r) }) {
		return nil, fmt.Errorf("%w: audience %v is not this resource", ErrInvalidToken, []string(aud))
	}
	sub, _ := claims.GetSubject()
	if sub == "" {
		return nil, fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	scopes := scopesOf(claims)
	for _, s := range v.cfg.Scopes {
		if !slices.Contains(scopes, s) {
			return nil, fmt.Errorf("%w: scope %q is required", ErrInsufficientScope, s)
		}
	}
	return &Token{Subject: sub, Scopes: scopes, Claims: claims}, nil
}

// scopesOf reads "scope" (space-separated, RFC 9068) or "scp" (a list or
// a space-separated string, as some providers send).
func scopesOf(c jwt.MapClaims) []string {
	if s, ok := c["scope"].(string); ok {
		return strings.Fields(s)
	}
	if s, ok := c["scp"].(string); ok {
		return strings.Fields(s)
	}
	return Strings(c["scp"])
}

// Strings reads a claim that is a string or a list of strings.
func Strings(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// key returns the public key with the given id, fetching keys when they
// are stale or the id is unknown.
func (v *Verifier) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	k, ok := v.lookup(kid)
	stale := time.Since(v.fetched) > keyRefresh
	if (!ok || stale) && time.Since(v.lastAttempt) >= minFetchInterval {
		v.lastAttempt = time.Now()
		if err := v.fetch(ctx); err != nil {
			if !ok {
				return nil, err
			}
		} else {
			k, ok = v.lookup(kid)
		}
	}
	if !ok {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	return k, nil
}

func (v *Verifier) lookup(kid string) (crypto.PublicKey, bool) {
	if kid == "" && len(v.keys) == 1 {
		for _, k := range v.keys {
			return k, true
		}
	}
	k, ok := v.keys[kid]
	return k, ok
}

// fetch loads the JWKS, discovering its URL from the issuer's metadata
// the first time.
func (v *Verifier) fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if v.jwksURL == "" {
		u, err := v.discover(ctx)
		if err != nil {
			return err
		}
		v.jwksURL = u
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := v.getJSON(ctx, v.jwksURL, &set); err != nil {
		return fmt.Errorf("fetching JWKS: %w", err)
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		if pub, err := k.publicKey(); err == nil {
			keys[k.Kid] = pub
		}
	}
	if len(keys) == 0 {
		return errors.New("the JWKS has no usable signing keys")
	}
	v.keys, v.fetched = keys, time.Now()
	return nil
}

// discover finds the JWKS URL in the issuer's OpenID Connect or OAuth
// authorization server metadata (RFC 8414).
func (v *Verifier) discover(ctx context.Context) (string, error) {
	issuer := strings.TrimSuffix(v.cfg.Issuer, "/")
	candidates := []string{issuer + "/.well-known/openid-configuration"}
	if scheme, rest, ok := strings.Cut(issuer, "://"); ok {
		host, path, _ := strings.Cut(rest, "/")
		if path != "" {
			path = "/" + path
		}
		candidates = append(candidates, scheme+"://"+host+"/.well-known/oauth-authorization-server"+path)
	}
	var errs []error
	for _, u := range candidates {
		var meta struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, u, &meta); err != nil {
			errs = append(errs, err)
			continue
		}
		if strings.TrimSuffix(meta.Issuer, "/") != issuer {
			return "", fmt.Errorf("metadata at %s names issuer %q, not %q", u, meta.Issuer, v.cfg.Issuer)
		}
		if meta.JWKSURI == "" {
			return "", fmt.Errorf("metadata at %s has no jwks_uri", u)
		}
		return meta.JWKSURI, nil
	}
	return "", fmt.Errorf("discovering the issuer's keys: %w", errors.Join(errs...))
}

func (v *Verifier) getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// jwk is one JSON Web Key (RFC 7517) of the kinds used to sign tokens.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	b64 := base64.RawURLEncoding.DecodeString
	switch k.Kty {
	case "RSA":
		n, err1 := b64(k.N)
		e, err2 := b64(k.E)
		if err := errors.Join(err1, err2); err != nil {
			return nil, err
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("RSA key shorter than 2048 bits")
		}
		return pub, nil
	case "EC":
		curve := map[string]elliptic.Curve{"P-256": elliptic.P256(), "P-384": elliptic.P384(), "P-521": elliptic.P521()}[k.Crv]
		if curve == nil {
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err1 := b64(k.X)
		y, err2 := b64(k.Y)
		if err := errors.Join(err1, err2); err != nil {
			return nil, err
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) > size || len(y) > size {
			return nil, errors.New("EC coordinates too long")
		}
		point := make([]byte, 1+2*size)
		point[0] = 4
		copy(point[1+size-len(x):], x)
		copy(point[1+2*size-len(y):], y)
		return ecdsa.ParseUncompressedPublicKey(curve, point)
	case "OKP":
		x, err := b64(k.X)
		if err != nil {
			return nil, err
		}
		if k.Crv != "Ed25519" || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("unsupported OKP key %q", k.Crv)
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, fmt.Errorf("unsupported key type %q", k.Kty)
}
