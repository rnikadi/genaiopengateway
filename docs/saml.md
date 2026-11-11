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

The groups attribute holds the names that `sso.roles` mappings match. With SCIM provisioning, a provisioned user's directory groups are used instead.

## Okta

1. Create an app integration with **SAML 2.0**.
2. Set **Single sign-on URL** to the ACS URL, and **Audience URI** to the entity id.
3. Set **Name ID format** to EmailAddress.
4. Add the attribute statements `email` = `user.email` and `displayName` = `user.displayName`.
5. Add a group attribute statement `groups`, with a filter that matches the groups `sso.roles` maps.
6. Copy the **Metadata URL** into `idp_metadata_url`.

## Microsoft Entra ID

1. In the enterprise application, open **Single sign-on** and choose **SAML**.
2. Set **Identifier** to the entity id, and **Reply URL** to the ACS URL.
3. Under **Attributes & Claims**, add a group claim. Choose **Groups assigned to the application** and emit **Cloud-only group display names** (or `sAMAccountName` for synced groups), so the values match `sso.roles`.
4. Set `groups_attribute` to `http://schemas.microsoft.com/ws/2008/06/identity/claims/groups`.
5. Copy the **App Federation Metadata Url** into `idp_metadata_url`.

The email is read from Entra ID's `emailaddress` claim.

## Keycloak

1. Create a client with protocol **saml** and client id equal to the entity id.
2. Set **Valid redirect URIs** to the ACS URL.
3. Turn off **Client signature required**, since the gateway does not sign its requests.
4. Add a **Group list** mapper named `groups`, with **Full group path** off.
5. Use `https://<keycloak>/realms/<realm>/protocol/saml/descriptor` as `idp_metadata_url`.

## Security

- **Signatures:** the response or its assertion must be signed by a certificate in the identity provider's metadata. SHA-1 signatures are refused.
- **Request binding:** the response must answer the request this browser started. The request id is kept in a short-lived cookie, so unsolicited, identity-provider-initiated sign-ins are refused. Start from the gateway's sign-in page or from `https://<gateway>/ui/auth/saml/login`.
- **Replay:** each assertion signs in once. Its id is recorded in the database (`saml_assertions`, migration 0018) until it expires, so a captured response cannot be replayed on any replica.
- **Audience, destination and validity window:** these are checked as SAML requires.
- **Metadata refresh:** the metadata is read when the first sign-in starts, and again when the settings change. To pick up a rotated signing certificate, save the settings or restart.
- **Cookies:** over HTTPS the flow cookie is `SameSite=None; Secure`, so that the identity provider's cross-site POST carries it.

## Not supported

- **Signed or encrypted AuthnRequests and encrypted assertions:** the gateway has no key pair of its own. Turn off assertion encryption at the identity provider. TLS protects the assertion in transit.
- **Single logout:** signing out of the gateway ends its session only.
- **The MCP connect flow's identity check** ([mcp-user-credentials.md](mcp-user-credentials.md)) needs OIDC.
