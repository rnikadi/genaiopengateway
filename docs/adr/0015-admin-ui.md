# 0015. Admin UI: a single-page client of the admin API, embedded in the binary

- Status: accepted
- Date: 2026-10-23

## Context

Operators need a browser UI for tenants, keys, usage, MCP approvals and audit. The gateway is one Go binary with a small, reviewed dependency set (security.md, supply chain). Options:

1. **Server-rendered pages** (Go templates with htmx). No JavaScript toolchain, but a second set of handlers beside the admin API, each needing its own authorization checks.
2. **A React or Vue app built with Node.** Familiar to many contributors, but adds Node, a bundler and hundreds of npm packages to every build and every security review.
3. **A single-page app with no build step**, written as plain ES modules with Preact and htm (about 5 KB together), vendored and embedded in the binary.

## Decision

Option 3. The UI is a static single-page app in `ui/`, embedded with `go:embed` and served at `/ui/`. It talks only to the admin API, with a session cookie.

- Preact and htm are vendored as pinned ES module files with recorded checksums. There is no `package.json` and no bundler; Go is the only toolchain.
- Charts are small SVG components written for the UI; no chart library.
- Styling is one hand-written CSS file with light and dark themes.
- The UI does nothing the admin API does not offer. Authorization lives in the API.

## Consequences

- One authorization point: the UI cannot grant what the API refuses, and every feature the UI has is scriptable.
- Builds, CI and the Docker image stay Go-only. The UI's whole dependency surface is two small files that can be read in full.
- No TypeScript or JSX compile step: components use htm's tagged templates. That is less familiar than JSX, but readable and debuggable in the browser as written.
- If the UI outgrows this setup, it can move to a bundled build later without changing the API.
