package server

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"

	"github.com/russellhaering/gosaml2/types"
)

// Identity providers pick the OAEP digest and mask generation function
// separately; Keycloak's default is a SHA-256 digest with MGF1-SHA-1, which
// the upstream gosaml2 could not decrypt (third_party/README.md).
func TestSAMLKeyTransportDigests(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	aesKey := make([]byte, 32)
	_, _ = rand.Read(aesKey)

	for _, tc := range []struct {
		name, alg, digest, mgf string
		hash, mgfHash          crypto.Hash
		wantErr                string
	}{
		{"mgf1p without a digest", types.MethodRSAOAEP, "", "", crypto.SHA1, crypto.SHA1, ""},
		{"mgf1p with SHA-256 (Keycloak)", types.MethodRSAOAEP, types.MethodXMLEncSHA256, "", crypto.SHA256, crypto.SHA1, ""},
		{"mgf1p with xmldsig SHA-256", types.MethodRSAOAEP, types.MethodSHA256, "", crypto.SHA256, crypto.SHA1, ""},
		{"mgf1p with SHA-512", types.MethodRSAOAEP, types.MethodXMLEncSHA512, "", crypto.SHA512, crypto.SHA1, ""},
		{"rsa-oaep, default MGF", types.MethodRSAOAEP2, types.MethodXMLEncSHA256, "", crypto.SHA256, crypto.SHA1, ""},
		{"rsa-oaep with MGF1-SHA-256", types.MethodRSAOAEP2, types.MethodXMLEncSHA256, types.MethodMGF1SHA256, crypto.SHA256, crypto.SHA256, ""},
		{"rsa-oaep with MGF1-SHA-512", types.MethodRSAOAEP2, types.MethodXMLEncSHA512, types.MethodMGF1SHA512, crypto.SHA512, crypto.SHA512, ""},
		{"an unknown digest", types.MethodRSAOAEP, "urn:md5", "", crypto.SHA1, crypto.SHA1, "unsupported digest"},
		{"an unknown MGF", types.MethodRSAOAEP2, "", "urn:mgf", crypto.SHA1, crypto.SHA1, "unsupported mask"},
		{"a mismatched MGF", types.MethodRSAOAEP2, types.MethodXMLEncSHA256, types.MethodMGF1SHA1, crypto.SHA256, crypto.SHA256, "rsa internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ct, err := rsa.EncryptOAEPWithOptions(rand.Reader, &key.PublicKey, aesKey, &rsa.OAEPOptions{Hash: tc.hash, MGFHash: tc.mgfHash})
			if err != nil {
				t.Fatal(err)
			}
			ek := types.EncryptedKey{CipherValue: base64.StdEncoding.EncodeToString(ct), EncryptionMethod: types.EncryptionMethod{Algorithm: tc.alg}}
			if tc.digest != "" {
				ek.EncryptionMethod.DigestMethod = &types.DigestMethod{Algorithm: tc.digest}
			}
			if tc.mgf != "" {
				ek.EncryptionMethod.MGF = &types.DigestMethod{Algorithm: tc.mgf}
			}
			_, err = ek.DecryptSymmetricKey(cert)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("decrypt: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}
