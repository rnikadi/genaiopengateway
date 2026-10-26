# 0029: Sign-in roles resolve to one organization and a verified email

- Status: accepted
- Date: 2026-10-26

## Context

The October 2026 walkthrough and pentest found two ways for a sign-in to get the wrong role.

- Role mappings name an organization by name or id. Organization names were not unique, and an organization's admin could rename it to any name. With two organizations of one name, a mapping resolved to whichever the gateway found first: reproduced on the Compose stack, where `admin@example.com` became admin of a new, empty `acme` and lost the real one. An admin could also rename their organization to a name mapped for another organization, or one the owner had mapped but not yet created, and so pull those users in.
- A mapping by `email` matched the ID token's `email` claim whether or not the identity provider had verified it. Providers that let users register with any address would hand out the mapped role, `owner` included.

Names also had no limits: a whitespace-only name and a 900 KB name were accepted.

## Decision

- Organization names are unique, compared without case. Migration 0015 renames any existing duplicate, except the oldest, to `name (org id)` and adds a unique index, so an upgrade cannot fail on old data and the oldest organization keeps the name a mapping most likely meant.
- Only owners rename an organization. Admins still edit the rest of what they could.
- Names of organizations, teams and keys are trimmed, 1 to 200 characters, without control characters.
- An `email` mapping matches only when the ID token does not say the email is unverified: `email_verified` false (or the string `"false"`) skips email mappings for that sign-in. Providers that never send the claim, such as Entra ID, keep working; group mappings are unaffected.

## Consequences

- Creating or renaming an organization to a taken name returns `409`.
- An upgraded instance with duplicate names shows the renamed ones with their id in the name. Owners rename them as they like.
- Users whose provider marks their email unverified lose email-mapped roles until it is verified. Group mappings are the recommended way to grant roles.
