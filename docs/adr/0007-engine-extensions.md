# ADR-0007: engine extensions in the adapters file

Status: **Accepted** (2026-11-13). Refines UP-ADR-002 §2 and ADR-0003; applies PL-ADR-001 R4.

## Context

Engines need declarative config that is not a port: test-engine inbox sources (name, secret reference, accepted event types, trust) live in an env-named JSON file (`TE_INBOX_SOURCES`, test-engine ADR-0025) because `adapters.Parse` refuses unknown keys. Config outside the adapters file is invisible to `GET /v1/adapters` and to `adapters plan`, so a weakening change (a new source trusted as `dispatched`) cannot be reviewed.

## Decision

1. **One section, keyed by engine-declared names.** `extensions: {<key>: <value>}` in `adapters.yaml`. `Spec.Extensions[key]` declares each key with an `adapters.Extension`; a key the spec does not declare is a `Check` error. The file stays strict: only declared keys pass.
2. **Schema, Discover and Weakening are required.** `Check` refuses a declared extension whose `Schema` (JSON Schema draft 2020-12, no remote refs) does not compile or that lacks `Discover` or `Weakening`, even when the file does not use it, so a broken spec fails at boot. A used value must satisfy `Schema` (`host.ValidateJSON`, the validator of plugin `config_schema`). Engine semantic rules (for example "a source accepting `test.attempt.*` needs `trust: dispatched`") go in `Spec.Validate`; boot and `adapters check` run the same `Check`, so preflight equals boot.
3. **Secrets are references, and discovery is an allowlist.** The ADR-0003 §6 rule covers the whole value, through maps and lists, and a string with `password=`, `pwd=`, `Bearer ` or `Basic ` is refused like a URL with credentials. Nothing reaches `GET /v1/adapters` by default: the engine's `Discover(raw)` returns what is shown. Its output is refused (a `Check` error, and absent from discovery) when a key looks secret (`secret`, `token`, `pass`, `auth`, `signing`, `dsn`, `cookie`, `session`, `…key`, …), when a `*Ref` key does not hold a reference, or when a string carries credentials. Each `*Ref` value is shown as `redacted:<first 4 bytes of its sha256, hex>`.
4. **Plan.** `Diff` compares the desired projection with the served one as canonical JSON; a difference is a change on port `extensions.<key>` with the compact JSON as `from`/`to`. `Weakening(from, to)` receives both projections (`nil` when absent): the running engine serves only its projection, so `Discover` must keep every field the classifier needs. A desired value that cannot be projected is a weakening change carrying the error; `Diff` called with no classifier marks every change weakening. A rotated `*Ref` adds a `notes` entry ("credential reference changed at <path>") without its value and without being a weakening.
5. **`Change.notes`** is a new optional field of the plan JSON; the text plan prints `[note: …]`.
6. **Env alias.** `<P>_EXTENSION_<KEY>` carries the value as JSON (or YAML) without a file, in dev only: production refuses it (extensions must be reviewable), and a key set in both the file and env is an error. `EnvNames` lists it.
7. **Engine API.** Declare `Spec.Extensions`; read with `File.Extension(key, &v)` (JSON tags); serve through `Plugins.Describe`. A nil instance makes `host.ValidateJSON` compile the schema only. The wire contract `api/acplug/v1` does not change.

## Consequences

- test-engine can move inbox sources into `extensions.inbox` and drop `TE_INBOX_SOURCES`; plan then flags a new `dispatched` source.
- `host.ValidateJSON(schema, instance []byte)` is exported; the plugin config check uses it too.
