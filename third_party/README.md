# Third-party code carried with patches

## gosaml2

`gosaml2/` is [github.com/russellhaering/gosaml2](https://github.com/russellhaering/gosaml2) v0.12.0 (Apache 2.0, see its `LICENSE`), without its tests and examples, used through a `replace` in `go.mod`. It differs from upstream in `types/encrypted_key.go` only:

- RSA-OAEP key transport takes the digest and the mask generation function separately. `rsa-oaep-mgf1p` always uses MGF1 with SHA-1, and `xmlenc11#rsa-oaep` names its MGF. Upstream used the digest for both, so assertions encrypted with Keycloak's defaults (a SHA-256 digest with MGF1-SHA-1) did not decrypt.
- The XML Encryption digest URIs (`xmlenc#sha256`, `xmlenc#sha512`, `xmldsig-more#sha384`) are accepted besides the `xmldsig#` ones.

`TestSAMLKeyTransportDigests` in `internal/server` covers the patch, and the Keycloak SAML run in `.github/workflows/idp.yml` covers it end to end. The change is meant for upstream; drop the copy and the `replace` once a release has it.
