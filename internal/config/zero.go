package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DefaultDataDir is where a zero-config start keeps its database and
// generated secrets, unless GOG_DATA_DIR says otherwise.
const DefaultDataDir = "data"

// bootstrapFile holds the secrets a zero-config start generated.
const bootstrapFile = "bootstrap.json"

// Bootstrap describes a zero-config start (docs/quickstart.md): the data
// directory, the generated admin token, and whether this start created it.
type Bootstrap struct {
	DataDir    string
	File       string
	AdminToken Secret
	Created    bool
}

type bootstrapSecrets struct {
	AdminToken    string    `json:"admin_token"`
	EncryptionKey string    `json:"encryption_key"`
	CreatedAt     time.Time `json:"created_at"`
}

// ZeroConfig builds the config for a start without a config file: SQLite
// in dataDir, in-memory limits, the admin API and UI on, and an admin
// token and encryption key generated once and kept in dataDir with
// owner-only permissions. Providers, models and keys are then added from
// the admin UI's setup wizard and stored in the database, so the result
// is the same as a config file with only those settings.
func ZeroConfig(dataDir string) (*Config, *Bootstrap, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create data directory: %w", err)
	}
	b := &Bootstrap{DataDir: dataDir, File: filepath.Join(dataDir, bootstrapFile)}
	var s bootstrapSecrets
	raw, err := os.ReadFile(b.File)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", b.File, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		s = bootstrapSecrets{AdminToken: randomHex(32), EncryptionKey: randomBase64(32), CreatedAt: time.Now().UTC()}
		out, _ := json.MarshalIndent(s, "", "  ")
		// O_EXCL so two processes starting together cannot both write.
		f, err := os.OpenFile(b.File, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("write %s: %w", b.File, err)
		}
		_, werr := f.Write(append(out, '\n'))
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, nil, fmt.Errorf("write %s: %w", b.File, werr)
		}
		b.Created = true
	default:
		return nil, nil, fmt.Errorf("read %s: %w", b.File, err)
	}
	if len(s.AdminToken) < MinAdminTokenLength || !validKey(s.EncryptionKey) {
		return nil, nil, fmt.Errorf("%s is damaged: remove it to generate new secrets (stored provider keys will no longer decrypt)", b.File)
	}
	b.AdminToken = Secret(s.AdminToken)
	dsn := "file:" + filepath.ToSlash(filepath.Join(dataDir, "gateway.db"))
	// Quoted so no character in the path can break the YAML.
	yml := fmt.Sprintf("admin: {token: %s}\nstore: {driver: sqlite, dsn: %s}\nsecrets: {encryption_key: %s}\n",
		strconv.Quote(s.AdminToken), strconv.Quote(dsn), strconv.Quote(s.EncryptionKey))
	c, err := Parse([]byte(yml))
	if err != nil {
		return nil, nil, err
	}
	return c, b, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomBase64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}
