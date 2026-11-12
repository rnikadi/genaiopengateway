# Identity providers

Operators sign in to the admin UI through your identity provider, with OIDC ([admin-ui.md](admin-ui.md#with-oidc)) or SAML 2.0 ([saml.md](saml.md)). This page has step-by-step setup for the common identity providers, and says how each one is tested.

| Identity provider | OIDC | SAML | SCIM | Tested |
| --- | --- | --- | --- | --- |
| Keycloak | Yes | Yes, with signed requests, encrypted assertions and single logout | No | On every change to sign-in code, in a browser, against Keycloak 26 |
| Okta | Yes | Yes | Yes | Nightly, once the project's test tenant is connected |
| Microsoft Entra ID | Yes | Yes | Yes | Nightly, once the project's test tenant is connected |
| Google Workspace | Yes | Yes | No | Nightly (OIDC), once the project's test account is connected |
| PingOne | Yes | Yes | Yes | Nightly, once the project's test tenant is connected |
| Any other | Yes, with standard discovery | Yes, with metadata | Any SCIM 2.0 client | The same code paths; tell us how it went |

In every case, the gateway needs:

- **A group claim or attribute** that holds the names `sso.roles` maps to roles. Without one, nobody has a role.
- **The redirect or ACS URL** registered exactly: `https://<gateway>/ui/auth/callback` for OIDC, `https://<gateway>/ui/auth/saml/acs` for SAML.

## Keycloak

**OIDC.** Create a client with **Client authentication** on and **Standard flow** only. Set **Valid redirect URIs** to the callback URL. Add a **Group Membership** mapper named `groups` with **Full group path** off. Use `https://<keycloak>/realms/<realm>` as the issuer.

**SAML.** See [saml.md](saml.md#keycloak). With the gateway's key pair, Keycloak checks the request signatures, encrypts assertions and does single logout.

The test realm in `test/idp/keycloak/realm.json` is a working example of both clients.

## Okta

**OIDC.** In **Applications**, create an app integration with **OIDC** and **Web Application**. Set **Sign-in redirect URIs** to the callback URL and **Assignments** to the groups that need access. On the **Sign On** tab, edit **OpenID Connect ID Token** and set **Groups claim filter** to `groups`, matches regex `.*` (or a narrower filter). Use `https://<your-okta-domain>` as the issuer, or the authorization server's issuer if you use a custom one.

**SAML.** See [saml.md](saml.md#okta).

**SCIM.** See [scim.md](scim.md).

## Microsoft Entra ID

**OIDC.** In **App registrations**, register an app with the callback URL as a **Web** redirect URI. Under **Certificates & secrets**, add a client secret. Under **Token configuration**, add a groups claim; choose **Groups assigned to the application**, and for the ID token emit **sAMAccountName** for synced groups or the group's **display name** for cloud-only groups (the default, the object id, works too if `sso.roles` names ids). Use `https://login.microsoftonline.com/<tenant-id>/v2.0` as the issuer. Under **API permissions**, `openid`, `email` and `profile` are enough.

Users in more than 200 groups get a group overage claim instead of their groups. Assign groups to the application and choose **Groups assigned to the application**, which avoids it.

**SAML.** See [saml.md](saml.md#microsoft-entra-id).

**SCIM.** See [scim.md](scim.md).

## Google Workspace

**OIDC.** In the Google Cloud console, under **APIs & Services**, **Credentials**, create an **OAuth client ID** of type **Web application** with the callback URL as an authorized redirect URI. Use `https://accounts.google.com` as the issuer.

Google's ID tokens carry no groups. Map roles by email instead (`sso.roles` entries with `email:`), or use SAML, where Google can send group membership:

**SAML.** In the Admin console, under **Apps**, **Web and mobile apps**, add a **custom SAML app**. Set **ACS URL** to the ACS URL and **Entity ID** to the gateway's entity id, with **Name ID format** EMAIL. Under **Group membership**, add the groups and name the app attribute `groups`. Use **Download metadata** and paste the XML into `idp_metadata`, since Google has no metadata URL. Google does not do single logout.

## PingOne

**OIDC.** In **Applications**, add an **OIDC Web App**. Set the redirect URI to the callback URL, and on **Attribute Mappings** add `groups` from **Group Names**. Use the application's **Issuer ID** (`https://auth.pingone.<region>/<environment-id>/as`) as the issuer.

**SAML.** Add a **SAML Application**, import the gateway's metadata from `https://<gateway>/ui/auth/saml/metadata`, and add attribute mappings `email` from **Email Address** and `groups` from **Group Names**. Copy the **IDP Metadata URL** into `idp_metadata_url`. With the gateway's key pair, turn on **Enforce signed AuthnRequest** and **Encrypt assertion**, and set the **SLO endpoint** to `https://<gateway>/ui/auth/saml/slo`.

## How sign-in is tested

`test/idp/run.sh <idp> <oidc|saml>` starts a gateway set up for the identity provider, and `test/e2e/idp-signin.mjs` drives a browser through the identity provider's own login page. It checks that the gateway knows the user and gives the mapped role, that signing out ends the session, and for SAML with single logout that the identity provider's session ended too, because it asks for the password again.

The `identity providers` workflow (`.github/workflows/idp.yml`) runs:

- **Keycloak**, in a container, for OIDC and SAML, on every pull request that touches sign-in code and every night. SAML runs with the gateway's key pair, so signed requests, encrypted assertions and single logout are tested end to end.
- **Okta, Entra ID, Google and PingOne**, every night, against the project's test tenants. Each identity provider and protocol pair runs when its secret is stored, and says it is not set up otherwise.

### Connecting a test tenant

The hosted runs need a test tenant with:

1. A test user **without MFA**, in a group named `gateway-owners` (or set `IDP_GROUP`; for Google OIDC, which sends no groups, set `IDP_ROLE_BY_EMAIL=1`). Use a dedicated tenant or developer account: the user's password is stored as a repository secret.
2. An app for each protocol, set up as above with these URLs, which the browser reaches on the test runner:
   - OIDC callback: `http://localhost:18090/ui/auth/callback`
   - SAML ACS: `http://localhost:18091/ui/auth/saml/acs`, entity id `http://localhost:18091/ui/auth/saml/metadata`, single logout `http://localhost:18091/ui/auth/saml/slo`
3. For SAML, a key pair made with `genaiopengateway saml-keygen`, with its certificate uploaded to the app.

Then add a repository secret per identity provider and protocol, named `IDP_<NAME>_<PROTOCOL>_ENV` (for example `IDP_OKTA_SAML_ENV`), holding:

```sh
# both protocols
IDP_USER=gateway-test@example.com
IDP_PASSWORD=...
IDP_GROUP=gateway-owners
# OIDC
IDP_ISSUER=https://example.okta.com
IDP_CLIENT_ID=...
IDP_CLIENT_SECRET=...
# SAML: PEM on one line is fine
IDP_METADATA_URL=https://example.okta.com/app/.../sso/saml/metadata
SAML_SP_CERT="-----BEGIN CERTIFICATE----- ... -----END CERTIFICATE-----"
SAML_SP_KEY="-----BEGIN PRIVATE KEY----- ... -----END PRIVATE KEY-----"
IDP_SINGLE_LOGOUT=1
```

Identity providers change their login pages now and then. When a nightly run fails at the login step, its screenshot is attached to the run, and the selectors to update are in `logins` in `test/e2e/idp-signin.mjs`.

## App galleries

Okta's Integration Network and Microsoft's Entra application gallery list apps that customers add in a few clicks, with SAML, OIDC and SCIM set up for them. The project plans to apply to both once the nightly tests have run green against their test tenants for a few weeks, which is what both reviews ask about. Until then, set the app up by hand as above.
