// Copyright 2016 Russell Haering et al.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// EncryptedKey contains the decryption key data from the saml2 core and xmlenc
// standards.
type EncryptedKey struct {
	// EncryptionMethod string `xml:"EncryptionMethod>Algorithm"`
	X509Data         string `xml:"KeyInfo>X509Data>X509Certificate"`
	CipherValue      string `xml:"CipherData>CipherValue"`
	EncryptionMethod EncryptionMethod
}

// EncryptionMethod specifies the type of encryption that was used.
type EncryptionMethod struct {
	Algorithm string `xml:",attr,omitempty"`
	//Digest method is present for algorithms like RSA-OAEP.
	//See https://www.w3.org/TR/xmlenc-core1/.
	//To convey the digest methods an entity supports,
	//DigestMethod in extensions element is used.
	//See http://docs.oasis-open.org/security/saml/Post2.0/sstc-saml-metadata-algsupport.html.
	DigestMethod *DigestMethod `xml:",omitempty"`
	// MGF is xmlenc11#rsa-oaep's mask generation function; without it,
	// MGF1 with SHA-1. (Patched in genaiopengateway, third_party/README.md.)
	MGF *DigestMethod `xml:"MGF,omitempty"`
}

// DigestMethod is a digest type specification
type DigestMethod struct {
	Algorithm string `xml:",attr,omitempty"`
}

// Well-known public-key encryption methods
const (
	MethodRSAOAEP  = "http://www.w3.org/2001/04/xmlenc#rsa-oaep-mgf1p"
	MethodRSAOAEP2 = "http://www.w3.org/2009/xmlenc11#rsa-oaep"
	MethodRSAv1_5  = "http://www.w3.org/2001/04/xmlenc#rsa-1_5"
)

// Well-known private key encryption methods
const (
	MethodAES128GCM    = "http://www.w3.org/2009/xmlenc11#aes128-gcm"
	MethodAES192GCM    = "http://www.w3.org/2009/xmlenc11#aes192-gcm"
	MethodAES256GCM    = "http://www.w3.org/2009/xmlenc11#aes256-gcm"
	MethodAES128CBC    = "http://www.w3.org/2001/04/xmlenc#aes128-cbc"
	MethodAES256CBC    = "http://www.w3.org/2001/04/xmlenc#aes256-cbc"
	MethodTripleDESCBC = "http://www.w3.org/2001/04/xmlenc#tripledes-cbc"
)

// Well-known hash methods
const (
	MethodSHA1   = "http://www.w3.org/2000/09/xmldsig#sha1"
	MethodSHA256 = "http://www.w3.org/2000/09/xmldsig#sha256"
	MethodSHA512 = "http://www.w3.org/2000/09/xmldsig#sha512"

	// The digest URIs XML Encryption defines, which Keycloak and others
	// send. (Patched in genaiopengateway.)
	MethodXMLEncSHA256 = "http://www.w3.org/2001/04/xmlenc#sha256"
	MethodXMLEncSHA512 = "http://www.w3.org/2001/04/xmlenc#sha512"
	MethodXMLEncSHA384 = "http://www.w3.org/2001/04/xmldsig-more#sha384"
)

// Mask generation functions of xmlenc11#rsa-oaep. (Patched in genaiopengateway.)
const (
	MethodMGF1SHA1   = "http://www.w3.org/2009/xmlenc11#mgf1sha1"
	MethodMGF1SHA224 = "http://www.w3.org/2009/xmlenc11#mgf1sha224"
	MethodMGF1SHA256 = "http://www.w3.org/2009/xmlenc11#mgf1sha256"
	MethodMGF1SHA384 = "http://www.w3.org/2009/xmlenc11#mgf1sha384"
	MethodMGF1SHA512 = "http://www.w3.org/2009/xmlenc11#mgf1sha512"
)

// oaepHashes returns the OAEP digest and the MGF1 hash of an EncryptedKey.
// rsa-oaep-mgf1p always uses MGF1 with SHA-1, whatever its digest, and
// xmlenc11#rsa-oaep names its MGF, SHA-1 by default. The upstream code
// used the digest for both, so keys from Keycloak's defaults (SHA-256
// digest, MGF1 with SHA-1) did not decrypt. (Patched in genaiopengateway.)
func (em EncryptionMethod) oaepHashes() (crypto.Hash, crypto.Hash, error) {
	digest := crypto.SHA1
	if em.DigestMethod != nil {
		switch em.DigestMethod.Algorithm {
		case "", MethodSHA1:
		case MethodSHA256, MethodXMLEncSHA256:
			digest = crypto.SHA256
		case MethodXMLEncSHA384:
			digest = crypto.SHA384
		case MethodSHA512, MethodXMLEncSHA512:
			digest = crypto.SHA512
		default:
			return 0, 0, fmt.Errorf("unsupported digest algorithm: %v", em.DigestMethod.Algorithm)
		}
	}
	mgf := crypto.SHA1
	if em.Algorithm == MethodRSAOAEP2 && em.MGF != nil {
		switch em.MGF.Algorithm {
		case "", MethodMGF1SHA1:
		case MethodMGF1SHA224:
			mgf = crypto.SHA224
		case MethodMGF1SHA256:
			mgf = crypto.SHA256
		case MethodMGF1SHA384:
			mgf = crypto.SHA384
		case MethodMGF1SHA512:
			mgf = crypto.SHA512
		default:
			return 0, 0, fmt.Errorf("unsupported mask generation function: %v", em.MGF.Algorithm)
		}
	}
	return digest, mgf, nil
}

// SHA-1 is commonly used for certificate fingerprints (openssl -fingerprint and ADFS thumbprint).
// SHA-1 is sufficient for our purposes here (error message).
func debugKeyFp(keyBytes []byte) string {
	if len(keyBytes) < 1 {
		return ""
	}
	hashFunc := sha1.New()
	hashFunc.Write(keyBytes)
	sum := strings.ToLower(hex.EncodeToString(hashFunc.Sum(nil)))
	var ret string
	for idx := 0; idx+1 < len(sum); idx += 2 {
		if idx == 0 {
			ret += sum[idx : idx+2]
		} else {
			ret += ":" + sum[idx:idx+2]
		}
	}
	return ret
}

// DecryptSymmetricKey returns the private key contained in the EncryptedKey document
func (ek *EncryptedKey) DecryptSymmetricKey(cert *tls.Certificate) (cipher.Block, error) {
	if len(cert.Certificate) < 1 {
		return nil, fmt.Errorf("decryption tls.Certificate has no public certs attached")
	}

	// The EncryptedKey may or may not include X509Data (certificate).
	// If included, the EncryptedKey certificate:
	// - is FYI only (fail if it does not match the SP certificate)
	// - is NOT used to decrypt CipherData
	if ek.X509Data != "" {
		if encCert, err := base64.StdEncoding.DecodeString(ek.X509Data); err != nil {
			return nil, fmt.Errorf("error decoding EncryptedKey certificate: %v", err)
		} else if !bytes.Equal(cert.Certificate[0], encCert) {
			return nil, fmt.Errorf("key decryption attempted with mismatched cert, SP cert(%.11s), assertion cert(%.11s)",
				debugKeyFp(cert.Certificate[0]), debugKeyFp(encCert))
		}
	}

	cipherText, err := base64.StdEncoding.DecodeString(ek.CipherValue)
	if err != nil {
		return nil, err
	}

	switch pk := cert.PrivateKey.(type) {
	case *rsa.PrivateKey:
		switch ek.EncryptionMethod.Algorithm {
		case "":
			return nil, fmt.Errorf("missing encryption algorithm")
		case MethodRSAOAEP, MethodRSAOAEP2:
			digest, mgf, err := ek.EncryptionMethod.oaepHashes()
			if err != nil {
				return nil, err
			}
			pt, err := pk.Decrypt(rand.Reader, cipherText, &rsa.OAEPOptions{Hash: digest, MGFHash: mgf})
			if err != nil {
				return nil, fmt.Errorf("rsa internal error: %v", err)
			}

			b, err := aes.NewCipher(pt)
			if err != nil {
				return nil, err
			}

			return b, nil
		case MethodRSAv1_5:
			pt, err := rsa.DecryptPKCS1v15(rand.Reader, pk, cipherText)
			if err != nil {
				return nil, fmt.Errorf("rsa internal error: %v", err)
			}

			//From https://docs.oasis-open.org/security/saml/v2.0/saml-core-2.0-os.pdf the xml encryption
			//methods to be supported are from http://www.w3.org/2001/04/xmlenc#Element.
			//https://www.w3.org/TR/2002/REC-xmlenc-core-20021210/Overview.html#Element.
			//https://www.w3.org/TR/2002/REC-xmlenc-core-20021210/#sec-Algorithms
			//Sec 5.4 Key Transport:
			//The RSA v1.5 Key Transport algorithm given below are those used in conjunction with TRIPLEDES
			//Please also see https://www.w3.org/TR/xmlenc-core/#sec-Algorithms and
			//https://www.w3.org/TR/xmlenc-core/#rsav15note.
			b, err := aes.NewCipher(pt)
			if err != nil {
				return nil, err
			}

			return b, nil
		default:
			return nil, fmt.Errorf("unsupported encryption algorithm: %s", ek.EncryptionMethod.Algorithm)
		}
	}
	return nil, fmt.Errorf("no cipher for decoding symmetric key")
}
