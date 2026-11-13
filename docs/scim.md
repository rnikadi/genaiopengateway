# SCIM provisioning

The identity provider can provision the people who operate the gateway, and their groups, over SCIM 2.0 ([RFC 7644](https://www.rfc-editor.org/rfc/rfc7644)). For a provisioned user, two things change:

- **Groups come from the directory.** Role mappings in `sso.roles` match the user's provisioned groups, not the groups claim from their last sign-in. When someone is added to or removed from a group, their roles change on their next request, with no need to sign in again or wait for `session_ttl`.
- **Deprovisioning signs them out.** A user who is deactivated (`active: false`) or deleted loses every admin UI session at once, and a deactivated user cannot sign in.

Users who were never provisioned keep signing in with the groups claim, so SCIM can be turned on gradually. Users are matched to sign-ins by their verified email, against the SCIM `userName` or primary email, ignoring case.

SCIM covers operators of the admin UI. Applications keep using virtual keys, which SCIM does not touch.

## Turn it on

Set a bearer token of at least 32 random characters:

```yaml
sso:
  oidc: { ... }
  scim:
    token: ${SCIM_TOKEN}
```

or set it on the **Settings** page under **Single sign-on**. The endpoints are served at `https://<gateway>/scim/v2` while a token is set and a store is configured, and answer `404` otherwise.

Owners see what was provisioned on the **Directory** page, or with `GET /admin/v1/directory`. Every change the identity provider makes is in the audit log, with actor `scim` and actions `scim.user.create`, `scim.user.update`, `scim.user.delete`, `scim.group.create`, `scim.group.update` and `scim.group.delete`.

## Okta

1. In the gateway's app integration, on the **General** tab, enable **SCIM provisioning**.
2. On the **Provisioning** tab, set:
   - **SCIM connector base URL**: `https://<gateway>/scim/v2`
   - **Unique identifier field for users**: `userName`
   - **Supported provisioning actions**: Push New Users, Push Profile Updates and Push Groups
   - **Authentication Mode**: HTTP Header, with the token as the Bearer value
3. Under **To App**, enable Create Users, Update User Attributes and Deactivate Users.
4. On **Push Groups**, push the groups that `sso.roles` maps. Their Okta names must match the `group` values in the mappings.

## Microsoft Entra ID

1. In the enterprise application, open **Provisioning** and set the mode to **Automatic**.
2. Set:
   - **Tenant URL**: `https://<gateway>/scim/v2`
   - **Secret Token**: the token
3. Select **Test Connection**.
4. Under **Mappings**, keep the default user attributes (`userPrincipalName` to `userName`, `mail` to `emails[type eq "work"].value`, and `Switch([IsSoftDeleted]...)` to `active`) and enable group provisioning.
5. Assign the groups that `sso.roles` maps, and start provisioning.

Entra ID sends group display names, so the names in `sso.roles` are the names shown in Entra ID, not object ids. Entra ID often has no `email_verified` claim, so the gateway trusts its emails (ADR 0029).

## Keycloak and others

Any client that speaks SCIM 2.0 with a bearer token works. Keycloak needs a SCIM extension, such as the community `scim-for-keycloak`.

## What is supported

| Endpoint | Methods |
| --- | --- |
| `/scim/v2/Users` | `GET` (filter, `startIndex`, `count`), `POST` |
| `/scim/v2/Users/{id}` | `GET`, `PUT`, `PATCH`, `DELETE` |
| `/scim/v2/Groups` | `GET` (filter, `excludedAttributes=members`), `POST` |
| `/scim/v2/Groups/{id}` | `GET`, `PUT`, `PATCH`, `DELETE` |
| `/scim/v2/ServiceProviderConfig`, `/ResourceTypes`, `/Schemas` | `GET` |

- **Users** keep `userName` (unique, ignoring case), `externalId`, `displayName` (or `name.formatted`, or the given and family names), the primary email and `active`. Other attributes are accepted and ignored.
- **Groups** keep `displayName` (unique, ignoring case), `externalId` and `members`.
- **Filters** are a single `eq` on `userName`, `externalId`, `emails.value` or `displayName` for users, and on `displayName` or `externalId` for groups. These are the lookups identity providers make. Anything else answers `400 invalidFilter`.
- **PATCH** supports `add`, `replace` and `remove`, with or without a `path`, including `members[value eq "<id>"]` and `emails[type eq "work"].value`. Operation names are case-insensitive, and booleans may be sent as strings, as Entra ID does.
- **Not supported:** bulk operations, sorting, ETags and password changes. `ServiceProviderConfig` says so.
