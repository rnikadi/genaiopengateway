# 0009. Built-in RBAC plus Cedar for attribute-based policy

- Status: accepted (roles refined by [0016](0016-operator-auth.md) for v0.3 and [0032](0032-roles-are-permission-sets.md) for v0.11; Cedar implemented in v0.11, see [cedar.md](../cedar.md))
- Date: 2026-10-22

## Decision

Fixed roles (owner, admin, member, viewer) per organization cover most needs. Fine-grained rules (model by data classification, tool by user group, region pinning) use Cedar policies evaluated in-process via cedar-go. OPA was considered; Cedar is smaller, faster to evaluate and easier to analyze.

## Consequences

Policies are data stored per organization, versioned and audited.
