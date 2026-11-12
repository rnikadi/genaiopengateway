# SAML sign-in

Operators can sign in to the admin UI with a SAML 2.0 identity provider instead of OIDC, for directories that offer only SAML or where SAML is the company standard. Roles, sessions, SCIM provisioning and the audit log work the same as with OIDC ([admin-ui.md](admin-ui.md), [roles.md](roles.md), [scim.md](scim.md)).

`sso.saml` and `sso.oidc` are alternatives: set one of them.

## Configure the gateway

```yaml
sso:
  saml:
    idp_metadata_url: https://idp.example.com/app/xyz/sso/saml/metadata   # or idp_metadata: <XML>
    acs_url: https://gateway.example.com/ui/auth/saml/acs
    # entity_id: https://gateway.example.com/ui/auth/saml/metadata      # default
    # email_attribute: email     # default: email, mail or the Entra ID email claim, else the NameID
    # name_attribute: displayName
    groups_attribute: groups      # default
    # The gateway's own key pair: signed requests, encrypted assertions and single logout.
    # sp_certificate: ${SAML_SP_CERT}
    # sp_private_key: ${SAML_SP_KEY}
    # require_encrypted_assertions: true
  roles:
    - {group: ai-platform, role: owner}
```

The same fields are on the **Settings** page under **SAML sign-in**.

The gateway is the service provider, and it publishes its metadata at `https://<gateway>/ui/auth/saml/metadata`:

| Setting | Value |
| --- | --- |
| Entity id (audience) | `https://<gateway>/ui/auth/saml/metadata`, or `entity_id` |
| Assertion consumer service (single sign-on URL) | `https://<gateway>/ui/auth/saml/acs`, HTTP-POST |
| NameID | Email address, or any persistent identifier when an email attribute is sent |
| Attributes | `email`, `displayName` and `groups` (the name in `groups_attribute`) |

With a key pair (below), the metadata also lists the gateway's signing and encryption certificate and its single logout service, `https://<gateway>/ui/auth/saml/slo` (HTTP-Redirect and HTTP-POST).

The groups attribute holds the names that `sso.roles` mappings match. With SCIM provisioning, a provisioned user's directory groups are used instead.

## The gateway's key pair

With a key pair of its own, the gateway:

- **signs its AuthnRequests** (RSA-SHA256 over the redirect binding), for identity providers that require signed requests;
- **takes encrypted assertions**, so the user's attributes are hidden from the browser and anything that logs the response. Set `require_encrypted_assertions: true` to refuse an assertion that is not encrypted. AES-GCM is listed first in the metadata; AES-CBC is accepted for identity providers that offer nothing else;
- **supports single logout**: signing out of the gateway also signs the user out of the identity provider, and signing out at the identity provider ends every gateway session of that user, on every replica.

Make a pair with the gateway binary (`/gateway` in the container image, for example `docker run --rm --user $(id -u) -v $PWD:/out -w /out --entrypoint /gateway ghcr.io/rnikadi/genaiopengateway saml-keygen`). It writes a 3072-bit RSA key and a self-signed certificate valid for five years. Identity providers trust the certificate because it is in the gateway's metadata, so it needs no certificate authority.

```sh
genaiopengateway saml-keygen -cert saml-sp.crt -key saml-sp.key
```

Put the two files' contents in `sp_certificate` and `sp_private_key` (environment variables work, as above), or paste them on the **Settings** page under **SAML sign-in**. The private key is stored encrypted, like every other secret. Then give the identity provider the gateway's metadata again, or upload the certificate where it asks for the service provider's signing or encryption certificate.

To rotate the pair, make a new one, update the settings and the identity provider together, and sign in once to check. Sign-ins started before the change may fail and need to be started again.

### Single logout

- **From the gateway:** **Sign out** ends the gateway session, then sends the browser to the identity provider's logout endpoint with a signed LogoutRequest for the same NameID and session index. The identity provider's LogoutResponse comes back to `/ui/auth/saml/slo` and lands on the sign-in page. The gateway session has already ended by then, so a slow or failing identity provider never leaves the user signed in to the gateway.
- **From the identity provider:** a LogoutRequest at `/ui/auth/saml/slo` must be signed by a certificate in the identity provider's metadata (SHA-1 is refused), be addressed to the gateway, and be issued within five minutes. It ends every gateway session of that NameID, records `operator.sign_out` in the audit log, and the gateway answers with a signed LogoutResponse.

Single logout needs a key pair and an identity provider whose metadata lists a logout endpoint with the HTTP-Redirect binding. Without either, signing out ends the gateway session only.

## Okta

1. Create an app integration with **SAML 2.0**.
2. Set **Single sign-on URL** to the ACS URL, and **Audience URI** to the entity id.
3. Set **Name ID format** to EmailAddress.
4. Add the attribute statements `email` = `user.email` and `displayName` = `user.displayName`.
5. Add a group attribute statement `groups`, with a filter that matches the groups `sso.roles` maps.
6. Copy the **Metadata URL** into `idp_metadata_url`.
7. With a key pair: under **Advanced settings**, set **Assertion Encryption** to Encrypted and upload the certificate; for single logout, check **Enable Single Logout**, set **Single Logout URL** to `https://<gateway>/ui/auth/saml/slo`, set **SP Issuer** to the entity id, and upload the certificate as the **Signature Certificate**.

## Microsoft Entra ID

1. In the enterprise application, open **Single sign-on** and choose **SAML**.
2. Set **Identifier** to the entity id, and **Reply URL** to the ACS URL.
3. Under **Attributes & Claims**, add a group claim. Choose **Groups assigned to the application** and emit **Cloud-only group display names** (or `sAMAccountName` for synced groups), so the values match `sso.roles`.
4. Set `groups_attribute` to `http://schemas.microsoft.com/ws/2008/06/identity/claims/groups`.
5. Copy the **App Federation Metadata Url** into `idp_metadata_url`.

The email is read from Entra ID's `emailaddress` claim.

With a key pair: set **Logout Url** to `https://<gateway>/ui/auth/saml/slo`. For encrypted assertions, upload the certificate under **SAML Certificates**, **Token encryption**, and activate it. Entra ID does not sign its logout messages with the redirect binding in every tenant; if single logout from Entra ID is refused, the gateway's log says why.

## Keycloak

1. Create a client with protocol **saml** and client id equal to the entity id.
2. Set **Valid redirect URIs** to the ACS URL.
3. Without a key pair, turn off **Client signature required**, since the gateway does not sign its requests. With one, leave it on, and on the **Keys** tab import the certificate for signing; turn on **Encrypt assertions** and import the same certificate for encryption.
4. Add a **Group list** mapper named `groups`, with **Full group path** off.
5. Use `https://<keycloak>/realms/<realm>/protocol/saml/descriptor` as `idp_metadata_url`.
6. For single logout, set **Logout Service Redirect Binding URL** to `https://<gateway>/ui/auth/saml/slo` and turn on **Front channel logout**.

## Security

- **Signatures:** the response or its assertion must be signed by a certificate in the identity provider's metadata. SHA-1 signatures are refused, including inside an encrypted assertion. (RSA-OAEP key transport with a SHA-1 digest, which most identity providers use to encrypt, is not a signature and is accepted.)
- **Request binding:** the response must answer the request this browser started. The request id is kept in a short-lived cookie, so unsolicited, identity-provider-initiated sign-ins are refused. Start from the gateway's sign-in page or from `https://<gateway>/ui/auth/saml/login`.
- **Replay:** each assertion signs in once. Its id is recorded in the database (`saml_assertions`, migration 0018) until it expires, so a captured response cannot be replayed on any replica.
- **Audience, destination and validity window:** these are checked as SAML requires.
- **Metadata refresh:** the metadata is read when the first sign-in starts, and again when the settings change. To pick up a rotated signing certificate, save the settings or restart.
- **Cookies:** over HTTPS the flow cookie is `SameSite=None; Secure`, so that the identity provider's cross-site POST carries it.

## Not supported

- **Encrypted NameIDs** in assertions or logout requests.
- **The SOAP binding** for single logout: logout messages go through the browser.
- **The MCP connect flow's identity check** ([mcp-user-credentials.md](mcp-user-credentials.md)) needs OIDC.
