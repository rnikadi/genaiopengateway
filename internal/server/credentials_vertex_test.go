package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/provider/vertex"
)

// A team's service account key makes its Vertex AI requests run as the
// team, in the team's project (docs/team-credentials.md).
func TestTeamCredentialsVertex(t *testing.T) {
	var mu sync.Mutex
	var paths, auths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths, auths = append(paths, r.URL.Path), append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`))
	}))
	t.Cleanup(up.Close)
	// Google's token endpoint: the token names the account that signed.
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		parts := strings.Split(r.PostForm.Get("assertion"), ".")
		var claims struct {
			Iss string `json:"iss"`
		}
		if len(parts) == 3 {
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			_ = json.Unmarshal(raw, &claims)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token-of-" + claims.Iss, "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(tokens.Close)
	old := vertex.TokenURL
	vertex.TokenURL = tokens.URL
	t.Cleanup(func() { vertex.TokenURL = old })

	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(pk)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	sa := func(fields map[string]string) string {
		m := map[string]string{"type": "service_account", "project_id": "team-project", "client_email": "team-a@team-project.iam.gserviceaccount.com", "private_key": pemKey, "private_key_id": "k1"}
		for k, v := range fields {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return string(b)
	}

	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
secrets: {encryption_key: "`+testEncryptionKey+`"}
providers:
  gcp: {type: vertex, base_url: "`+up.URL+`", project: shared-project, location: europe-west4, credentials_file: /nonexistent/key.json}
models:
  gem: {targets: [{provider: gcp, model: gemini-2.5-flash}]}
`)
	var org, a, b, keyA, keyB idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"a"}`, 201, &a)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"b"}`, 201, &b)
	e.call("POST", "/admin/v1/teams/"+a.ID+"/keys", `{"name":"k"}`, 201, &keyA)
	e.call("POST", "/admin/v1/teams/"+b.ID+"/keys", `{"name":"k"}`, 201, &keyB)
	cred := "/admin/v1/teams/" + a.ID + "/credentials/gcp"

	// What is refused: other credential types, another token endpoint,
	// a key that is no key, and an API key for a Vertex provider.
	for name, body := range map[string]string{
		"external account": `{"service_account":` + sa(map[string]string{"type": "external_account"}) + `}`,
		"authorized user":  `{"service_account":` + sa(map[string]string{"type": "authorized_user"}) + `}`,
		"own token uri":    `{"service_account":` + sa(map[string]string{"token_uri": "http://169.254.169.254/token"}) + `}`,
		"no private key":   `{"service_account":` + sa(map[string]string{"private_key": "nope"}) + `}`,
		"no project":       `{"service_account":` + sa(map[string]string{"project_id": ""}) + `}`,
		"not json":         `{"service_account":"not json"}`,
		"api key":          `{"api_key":"AIza-something"}`,
		"nothing":          `{}`,
	} {
		if rec := do(e.h, "PUT", cred, adminToken, body); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}

	var entry credentialEntry
	e.call("PUT", cred, `{"service_account":`+sa(nil)+`}`, 200, &entry)
	if !entry.Set || entry.Kind != "service_account" {
		t.Fatalf("entry: %+v", entry)
	}
	chat := func(key string) int {
		return do(e.h, "POST", "/v1/chat/completions", key, `{"model":"gem","messages":[{"role":"user","content":"hi"}]}`).Code
	}
	if code := chat(keyA.Key); code != 200 {
		t.Fatalf("team a = %d", code)
	}
	mu.Lock()
	path, auth := paths[len(paths)-1], auths[len(auths)-1]
	mu.Unlock()
	if !strings.Contains(path, "/projects/team-project/locations/europe-west4/") || auth != "Bearer token-of-team-a@team-project.iam.gserviceaccount.com" {
		t.Fatalf("team a called %s with %s", path, auth)
	}
	// Another team still uses the shared credential, which here does not exist.
	n := len(paths)
	if code := chat(keyB.Key); code == 200 || len(paths) != n {
		t.Fatalf("team b = %d, upstream calls %d", code, len(paths)-n)
	}

	// The key pasted as text, with a project that is not the key's.
	text, _ := json.Marshal(sa(nil))
	e.call("PUT", cred, `{"service_account":`+string(text)+`,"project":"billing-project"}`, 200, nil)
	if code := chat(keyA.Key); code != 200 || !strings.Contains(paths[len(paths)-1], "/projects/billing-project/") {
		t.Fatalf("with a project: %d %s", code, paths[len(paths)-1])
	}

	// Nothing of the key comes back, and the audit entry names the project only.
	list := do(e.h, "GET", "/admin/v1/teams/"+a.ID+"/credentials", adminToken, "").Body.String()
	audit := do(e.h, "GET", "/admin/v1/audit?limit=5", adminToken, "").Body.String()
	for _, secret := range []string{"PRIVATE KEY", "private_key", "team-a@team-project"} {
		if strings.Contains(list, secret) || strings.Contains(audit, secret) {
			t.Fatalf("%q is shown", secret)
		}
	}
	if !strings.Contains(list, `"kind":"service_account"`) || !strings.Contains(audit, "billing-project") {
		t.Fatalf("list %s audit %s", list, audit)
	}
	e.call("DELETE", cred, "", 204, nil)
	if code := chat(keyA.Key); code == 200 {
		t.Fatal("team a still reaches Vertex after removing its key")
	}
}
