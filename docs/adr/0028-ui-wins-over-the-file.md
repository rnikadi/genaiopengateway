# 0028: A change made in the UI wins over the config file for named objects

- Status: accepted (supersedes the "file wins" rule of [0023](0023-hybrid-config.md))
- Date: 2026-10-26

## Context

ADR 0023 merged stored objects into the config file, with the file winning on a name clash and file objects read-only in the UI. Operators bring an instance up from a config file and then want to run it from the admin UI: change a provider's endpoint, a model's price or a guardrail's action without editing the file and redeploying. ADR 0027 already let UI values win for gateway-wide settings. Ram asked for the same for every object.

## Decision

- A stored object with the same kind and name as one in the file replaces the file's object. Editing a file object in the UI stores such a copy.
- Deleting a file object stores a deletion marker (a spec of `null`), so the object does not come back from the file after a restart or reload.
- Reset removes the stored copy or marker, and the file's version applies again.
- A secret the operator leaves as `"set"` on a file object keeps the file's value, read from the file at each merge. It is not copied into the store, so a key from `${VAR}` keeps following the environment.
- Objects not changed in the UI keep following the file.

Importing the file into the store once, at first boot, was rejected: later edits to the file would then do nothing, silently, and teams that deploy their config from Git would lose that path.

## Consequences

- The running config can differ from the file. The Configuration page marks each changed or deleted file object, and Reset brings the file's version back.
- A file edit to an object changed in the UI has no effect until that object is reset.
- `409` on writes to file names goes away.
