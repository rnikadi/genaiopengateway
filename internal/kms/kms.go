// Package kms unwraps the gateway's encryption keys with a key
// management service: AWS KMS, Google Cloud KMS or Vault Transit
// (docs/secrets.md). It speaks each service's HTTP API directly, with
// the platform's usual credentials.
package kms

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"golang.org/x/oauth2/google"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// Client is the HTTP client for KMS calls; tests replace it.
var Client = &http.Client{Timeout: 20 * time.Second}

// serviceAccountToken is where Kubernetes mounts a pod's token.
var serviceAccountToken = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Unwrap decrypts a wrapped key. It is a config.Unwrapper.
func Unwrap(ctx context.Context, k config.KMS, wrapped string) ([]byte, error) {
	switch k.Provider {
	case config.KMSAWS:
		return unwrapAWS(ctx, k, wrapped)
	case config.KMSGCP:
		return unwrapGCP(ctx, k, wrapped)
	case config.KMSVault:
		return unwrapVault(ctx, k, wrapped)
	}
	return nil, fmt.Errorf("unknown KMS provider %q", k.Provider)
}

func unwrapAWS(ctx context.Context, k config.KMS, wrapped string) ([]byte, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("AWS credentials: %w", err)
	}
	region := cfg.Region
	// A key ARN names its region: arn:aws:kms:<region>:<account>:key/<id>.
	if parts := strings.Split(k.Key, ":"); len(parts) > 3 && parts[0] == "arn" && parts[3] != "" {
		region = parts[3]
	}
	if region == "" {
		return nil, errors.New("no AWS region: use a key ARN or set AWS_REGION")
	}
	endpoint := k.Endpoint
	if endpoint == "" {
		endpoint = "https://kms." + region + ".amazonaws.com/"
	}
	body, _ := json.Marshal(map[string]string{"CiphertextBlob": wrapped, "KeyId": k.Key})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "TrentService.Decrypt")
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return nil, fmt.Errorf("AWS credentials: %w", err)
	}
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(ctx, creds, req, hex.EncodeToString(sum[:]), "kms", region, time.Now()); err != nil {
		return nil, err
	}
	var out struct {
		Plaintext string `json:"Plaintext"`
	}
	if err := call(req, &out); err != nil {
		return nil, fmt.Errorf("AWS KMS Decrypt: %w", err)
	}
	return base64.StdEncoding.DecodeString(out.Plaintext)
}

func unwrapGCP(ctx context.Context, k config.KMS, wrapped string) ([]byte, error) {
	endpoint := k.Endpoint
	if endpoint == "" {
		endpoint = "https://cloudkms.googleapis.com"
	}
	body, _ := json.Marshal(map[string]string{"ciphertext": wrapped})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+"/v1/"+k.Key+":decrypt", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloudkms")
	if err != nil {
		return nil, fmt.Errorf("google credentials: %w", err)
	}
	tok, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("google credentials: %w", err)
	}
	tok.SetAuthHeader(req)
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	if err := call(req, &out); err != nil {
		return nil, fmt.Errorf("cloud KMS decrypt: %w", err)
	}
	return base64.StdEncoding.DecodeString(out.Plaintext)
}

func unwrapVault(ctx context.Context, k config.KMS, wrapped string) ([]byte, error) {
	addr := k.Endpoint
	if addr == "" {
		addr = os.Getenv("VAULT_ADDR")
	}
	if addr == "" {
		return nil, errors.New("no Vault address: set secrets.kms.endpoint or VAULT_ADDR")
	}
	addr = strings.TrimSuffix(addr, "/")
	token, err := vaultToken(ctx, addr, k)
	if err != nil {
		return nil, err
	}
	mount := k.Mount
	if mount == "" {
		mount = "transit"
	}
	body, _ := json.Marshal(map[string]string{"ciphertext": wrapped})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/v1/"+mount+"/decrypt/"+k.Key, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", token)
	if ns := os.Getenv("VAULT_NAMESPACE"); ns != "" {
		req.Header.Set("X-Vault-Namespace", ns)
	}
	var out struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := call(req, &out); err != nil {
		return nil, fmt.Errorf("vault transit decrypt: %w", err)
	}
	return base64.StdEncoding.DecodeString(out.Data.Plaintext)
}

// vaultToken is VAULT_TOKEN, or a token from a Kubernetes login when a
// role is set.
func vaultToken(ctx context.Context, addr string, k config.KMS) (string, error) {
	if k.Role == "" {
		if t := os.Getenv("VAULT_TOKEN"); t != "" {
			return t, nil
		}
		return "", errors.New("no Vault token: set VAULT_TOKEN, or secrets.kms.role to log in with Kubernetes")
	}
	jwt, err := os.ReadFile(serviceAccountToken)
	if err != nil {
		return "", fmt.Errorf("vault kubernetes login: %w", err)
	}
	mount := k.AuthMount
	if mount == "" {
		mount = "kubernetes"
	}
	body, _ := json.Marshal(map[string]string{"role": k.Role, "jwt": strings.TrimSpace(string(jwt))})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, addr+"/v1/auth/"+mount+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	if ns := os.Getenv("VAULT_NAMESPACE"); ns != "" {
		req.Header.Set("X-Vault-Namespace", ns)
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := call(req, &out); err != nil {
		return "", fmt.Errorf("vault kubernetes login: %w", err)
	}
	return out.Auth.ClientToken, nil
}

// call sends a request and decodes a JSON answer, or returns the
// service's error message.
func call(req *http.Request, out any) error {
	resp, err := Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode, msg)
	}
	return json.Unmarshal(b, out)
}
