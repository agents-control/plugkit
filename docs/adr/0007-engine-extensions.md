# ADR-0007: engine extensions in the adapters file

Status: **Accepted** (2026-11-13). Refines UP-ADR-002 §2 and ADR-0003; applies PL-ADR-001 R4.

## Context

Engines need declarative config that is not a port: test-engine inbox sources (name, secret reference, accepted event types, trust) live in an env-named JSON file (`TE_INBOX_SOURCES`, test-engine ADR-0025) because `adapters.Parse` refuses unknown keys. Config outside the adapters file is invisible to `GET /v1/adapters` and to `adapters plan`, so a weakening change (a new source trusted as `dispatched`) cannot be reviewed.

## Decision

1. **One section, keyed by engine-declared names.** `extensions: {<key>: <value>}` in `adapters.yaml`. `Spec.Extensions[key]` declares each key with an `adapters.Extension`; a key the spec does not declare is a `Check` error. The file stays strict: only declared keys pass.
2. **Schema.** `Extension.Schema` is a JSON Schema (draft 2020-12, no remote refs), required; `Check` validates the value with `host.ValidateJSON`, the same validator the host applies to plugin `config_schema`. Engine semantic rules (for example "a source accepting `test.attempt.*` needs `trust: dispatched`") go in `Spec.Validate`, which `Check` runs; boot and `adapters check` run the same `Check`, so preflight equals boot.
3. **Secrets are references, shown nowhere.** The ADR-0003 §6 rule applies to the whole value, through maps and lists: a key ending in `Ref` must hold a reference, a secret-looking key without `Ref` is an error, URLs with credentials are errors. In `GET /v1/adapters` every value under a `*Ref` or secret-looking key is replaced by `"redacted"`. The rule is plugkit's, not per engine, so no engine can opt out.
4. **Discovery.** `Document.extensions.<key>` holds the redacted projection: `Extension.Discover(raw)` when set (to narrow what operators see), the value itself otherwise. Discover output that does not marshal is a `Check` error.
5. **Plan.** `Diff` compares the desired projection with the served one; a difference is a change on port `extensions.<key>` with the compact JSON as `from`/`to`. `Extension.Weakening(from, to)` (both the redacted projections, `nil` when absent) returns the weakening reasons, so `adapters plan` flags them. A reference rotation is not a change.
6. **Env alias.** `<P>_EXTENSION_<KEY>` carries the value as JSON (or YAML) without a file, in dev only: production refuses it (extensions must be reviewable), and a key set in both the file and env is an error. `EnvNames` lists it.
7. **Engine API.** Declare `Spec.Extensions`; read with `File.Extension(key, &v)` (JSON tags); serve through `Plugins.Describe`. The wire contract `api/acplug/v1` does not change.

## Consequences

- test-engine can move inbox sources into `extensions.inbox` and drop `TE_INBOX_SOURCES`; plan then flags a new `dispatched` source.
- `host.ValidateJSON(schema, instance []byte)` is exported; the plugin config check uses it too.
