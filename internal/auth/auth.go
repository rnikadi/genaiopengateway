// Package auth issues gateway API keys and resolves them to principals.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"slices"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/policy"
)

const (
	// KeyPrefix starts every generated key.
	KeyPrefix = "sk-gog-"
	// keyRandomLen is the number of base62 characters after the prefix.
	keyRandomLen = 40
	// DisplayPrefixLen is how much of a key is stored for display.
	DisplayPrefixLen = len(KeyPrefix) + 4

	base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// NewKey returns a new random secret, its display prefix and its hash.
func NewKey() (secret, prefix, hash string, err error) {
	b := make([]byte, keyRandomLen)
	max := big.NewInt(int64(len(base62)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", "", "", err
		}
		b[i] = base62[n.Int64()]
	}
	secret = KeyPrefix + string(b)
	return secret, secret[:DisplayPrefixLen], Hash(secret), nil
}

// Hash returns the hex SHA-256 of a key. Keys are high-entropy, so a
// fast hash is sufficient and keeps lookups cheap.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Principal is the caller behind an API key, with the policies of every
// level of the tenancy hierarchy it belongs to.
type Principal struct {
	OrgID     string
	TeamID    string
	KeyID     string
	KeyName   string
	KeyPrefix string
	// Policies holds the org, team and key policies, in that order.
	Policies [3]policy.Policy
	// ExpiresAt is when the key stops working; nil means never.
	ExpiresAt *time.Time
}

// LogBodies reports whether bodies of this principal's requests are
// logged: the most specific level that sets log_bodies decides.
func (p *Principal) LogBodies() bool {
	for i := len(p.Policies) - 1; i >= 0; i-- {
		if v := p.Policies[i].LogBodies; v != nil {
			return *v
		}
	}
	return false
}

// Guardrails returns the guardrails named at any level, sorted and
// without duplicates.
func (p *Principal) Guardrails() []string {
	var out []string
	for _, pol := range p.Policies {
		out = append(out, pol.Guardrails...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Expired reports whether the key has expired at t.
func (p *Principal) Expired(t time.Time) bool {
	return p.ExpiresAt != nil && !t.Before(*p.ExpiresAt)
}

// AllowsModel reports whether every level permits the model.
func (p *Principal) AllowsModel(model string) bool {
	for _, pol := range p.Policies {
		if !pol.AllowsModel(model) {
			return false
		}
	}
	return true
}

// AllowsMCPServer reports whether every level permits the virtual MCP server.
func (p *Principal) AllowsMCPServer(name string) bool {
	for _, pol := range p.Policies {
		if !pol.AllowsMCPServer(name) {
			return false
		}
	}
	return true
}

// Directory is an immutable index of principals by key hash.
type Directory struct {
	byHash map[string]*Principal
	byID   map[string]*Principal
}

// NewDirectory builds a directory from a hash-to-principal map. The map
// must not be modified afterwards.
func NewDirectory(byHash map[string]*Principal) *Directory {
	byID := make(map[string]*Principal, len(byHash))
	for _, p := range byHash {
		byID[p.KeyID] = p
	}
	return &Directory{byHash: byHash, byID: byID}
}

// ByKeyID returns the principal of a key by its id, for work charged
// after the request that started it, such as a batch.
func (d *Directory) ByKeyID(id string) (*Principal, bool) {
	if d == nil || id == "" {
		return nil, false
	}
	p, ok := d.byID[id]
	return p, ok
}

// Lookup resolves a presented secret. Lookups go through the hash, so
// response timing does not depend on how much of a key matches.
func (d *Directory) Lookup(secret string) (*Principal, bool) {
	if secret == "" || d == nil {
		return nil, false
	}
	p, ok := d.byHash[Hash(secret)]
	return p, ok
}

// Len returns the number of keys.
func (d *Directory) Len() int { return len(d.byHash) }

// Static tenancy ids used for keys declared in the config file.
const (
	StaticOrgID  = "org_static"
	StaticTeamID = "team_static"
)

// StaticPrincipals converts config-file keys into directory entries.
func StaticPrincipals(keys []config.StaticKey) map[string]*Principal {
	out := make(map[string]*Principal, len(keys))
	for _, k := range keys {
		secret := k.Key.Value()
		prefix := secret
		if len(prefix) > DisplayPrefixLen {
			prefix = prefix[:DisplayPrefixLen]
		}
		out[Hash(secret)] = &Principal{
			OrgID:     StaticOrgID,
			TeamID:    StaticTeamID,
			KeyID:     "key_static_" + k.Name,
			KeyName:   k.Name,
			KeyPrefix: prefix,
			Policies:  [3]policy.Policy{{}, {}, k.Policy},
		}
	}
	return out
}
