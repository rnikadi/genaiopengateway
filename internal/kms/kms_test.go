package kms

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func TestAWS(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") != "TrentService.Decrypt" || !strings.Contains(r.Header.Get("Authorization"), "/eu-west-1/kms/aws4_request") {
			t.Errorf("request headers %v", r.Header)
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["CiphertextBlob"] != "d3JhcHBlZA==" || !strings.HasPrefix(in["KeyId"], "arn:aws:kms:eu-west-1:") {
			t.Errorf("body %v", in)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"Plaintext": base64.StdEncoding.EncodeToString(key)})
	}))
	defer srv.Close()
	got, err := Unwrap(context.Background(), config.KMS{Provider: "aws", Key: "arn:aws:kms:eu-west-1:111122223333:key/abc", Endpoint: srv.URL}, "d3JhcHBlZA==")
	if err != nil || string(got) != string(key) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestGCP(t *testing.T) {
	// Application Default Credentials from an authorized-user file, with
	// its token endpoint served here.
	var tokenURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"ya29.test","token_type":"Bearer","expires_in":3600}`))
		case strings.HasSuffix(r.URL.Path, ":decrypt"):
			if r.Header.Get("Authorization") != "Bearer ya29.test" || r.URL.Path != "/v1/projects/p/locations/global/keyRings/r/cryptoKeys/k:decrypt" {
				t.Errorf("%s %v", r.URL.Path, r.Header)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"plaintext": base64.StdEncoding.EncodeToString(key)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tokenURL = srv.URL + "/token"
	creds := filepath.Join(t.TempDir(), "adc.json")
	b, _ := json.Marshal(map[string]string{"type": "authorized_user", "client_id": "c", "client_secret": "s", "refresh_token": "r", "token_uri": tokenURL})
	if err := os.WriteFile(creds, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", creds)
	got, err := Unwrap(context.Background(), config.KMS{Provider: "gcp", Key: "projects/p/locations/global/keyRings/r/cryptoKeys/k", Endpoint: srv.URL}, "d3JhcHBlZA==")
	if err != nil || string(got) != string(key) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestVault(t *testing.T) {
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := serviceAccountToken
	serviceAccountToken = jwt
	t.Cleanup(func() { serviceAccountToken = prev })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/v1/auth/k8s/login":
			if in["role"] != "gateway" || in["jwt"] != "sa-jwt" {
				t.Errorf("login %v", in)
			}
			_, _ = w.Write([]byte(`{"auth":{"client_token":"hvs.login"}}`))
		case "/v1/kms/decrypt/gateway":
			if tok := r.Header.Get("X-Vault-Token"); tok != "hvs.env" && tok != "hvs.login" {
				t.Errorf("token %q", tok)
			}
			if in["ciphertext"] != "vault:v1:abc" {
				t.Errorf("decrypt %v", in)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"plaintext": base64.StdEncoding.EncodeToString(key)}})
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
		}
	}))
	defer srv.Close()
	t.Setenv("VAULT_TOKEN", "hvs.env")
	for _, k := range []config.KMS{
		{Provider: "vault", Key: "gateway", Endpoint: srv.URL, Mount: "kms"},
		{Provider: "vault", Key: "gateway", Endpoint: srv.URL, Mount: "kms", Role: "gateway", AuthMount: "k8s"},
	} {
		got, err := Unwrap(context.Background(), k, "vault:v1:abc")
		if err != nil || string(got) != string(key) {
			t.Fatalf("role %q: got %q, %v", k.Role, got, err)
		}
	}
	_, err := Unwrap(context.Background(), config.KMS{Provider: "vault", Key: "other", Endpoint: srv.URL}, "vault:v1:abc")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigUsesUnwrappedKey(t *testing.T) {
	calls := 0
	config.RegisterUnwrapper(func(_ context.Context, k config.KMS, wrapped string) ([]byte, error) {
		calls++
		return key, nil
	})
	t.Cleanup(func() { config.RegisterUnwrapper(Unwrap) })
	yml := []byte("secrets:\n  kms: {provider: vault, key: gw, wrapped_key: \"vault:v1:cfgtest\", previous_wrapped_keys: [\"vault:v1:old\"]}\n")
	for range 3 {
		c, err := config.Parse(yml)
		if err != nil {
			t.Fatal(err)
		}
		if c.Secrets.EncryptionKey.Value() != base64.StdEncoding.EncodeToString(key) || len(c.Secrets.PreviousKeys) != 1 {
			t.Fatalf("secrets %+v", c.Secrets)
		}
	}
	if calls != 2 {
		t.Fatalf("unwrapped %d times, want once per key", calls)
	}
	if _, err := config.Parse([]byte("secrets:\n  encryption_key: x\n  kms: {provider: vault, key: gw, wrapped_key: w}\n")); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v", err)
	}
}
