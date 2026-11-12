package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"time"
)

// runSAMLKeygen makes the gateway's SAML key pair: a 3072-bit RSA key and
// a self-signed certificate for it (docs/saml.md). Identity providers
// trust the certificate because it is in the gateway's metadata, so it
// needs no certificate authority.
func runSAMLKeygen(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("saml-keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	certOut := fs.String("cert", "saml-sp.crt", "file to write the certificate to")
	keyOut := fs.String("key", "saml-sp.key", "file to write the private key to")
	name := fs.String("name", "genaiopengateway", "the certificate's common name")
	years := fs.Int("years", 5, "how long the certificate is valid")
	storeFlags(fs) // accepted so that the global flags can come first
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *years < 1 || *years > 30 {
		return errors.New("-years must be between 1 and 30")
	}
	for _, f := range []string{*certOut, *keyOut} {
		if _, err := os.Stat(f); err == nil {
			return fmt.Errorf("%s exists; move it away first, so that a key in use is not replaced", f)
		}
	}
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: *name},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(*years, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*keyOut, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(*certOut, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Wrote the certificate to %s and the private key to %s (valid until %s).\n", *certOut, *keyOut, tmpl.NotAfter.Format("2006-01-02"))
	fmt.Fprintln(stdout, "Set sso.saml.sp_certificate and sso.saml.sp_private_key to their contents, then give the identity provider the gateway's metadata again.")
	return nil
}
