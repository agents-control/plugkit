# ADR-0001: plugkit is the shared host side of acplug/1 and imports no engine

Status: **Accepted** (2026-11-13). Implements UP-ADR-001 §3 and §10 (agents-control-research `state/specs/universal-plug/`), slice S0.

## Context

workflow-engine (`wfplugin/1`) and artifact-engine (`artplugin/1`) each carry a copy of the same plugin handshake, and the copies drifted (field names, credential form, HTTP mapping only in one). test-engine and metering-engine need the same machinery. UP-O1 converges the `Plugin` service on `acplug.v1.Plugin` and keeps the old manifests for one minor release. UP-O2 puts the shared code in its own module.

## Decision

1. **One module, these packages, this direction** (`scripts/check-deps.sh` enforces it):

   | Package | Owns | Imports |
   |---|---|---|
   | `api/acplug/v1` | `Plugin` (Manifest, Health), `Scope`, `Limits`, `EventSink` | generated |
   | `host` | dial, TLS/mTLS, bearer from a secret reference, HTTP/JSON mapping, handshake, pinning, capability narrowing, per-port guards | `api` |
   | `adapters` | `adapters.yaml` and env form, `Check`, `Diff` (plan), `Dial` with report verification, `Describe` | `host`, `conform`, `discovery` |
   | `discovery` | `GET /v1/adapters` document, handler, fetch | stdlib |
   | `conform` | runner, report, `Verify` over fixtures, embeddable CLI; `conform/corpus` broken plugins | `host`, `serve` |
   | `outbox` | batch `Sink` port and its plugin adapter; `outbox/sinktest` suite | `host`, `conform` |
   | `serve` | Go plugin helpers: `Plugin`, reference `MemorySink`, constant-time `Bearer`, loopback `Start` | `api`, `host` |

2. **plugkit imports no engine.** Engines import plugkit; their domain services (`wfplugin.v1.Transport`, `artplugin.v1.Format`, …) stay in each engine and reach plugins through `Client.Port(name)`.
3. **Old protocols are accepted through a hook, not through copies of the engines' protos.** `host.Legacy{Protocol, Service, Manifest}`: when `acplug.v1.Plugin/Manifest` answers `UNIMPLEMENTED`, the host tries each legacy hook. The engine owns the conversion of its old `ManifestResponse` to `acplug.v1.ManifestResponse` (port kind enum to port name, a `true` config schema where none was declared). Health is called at `/<Service>/Health` with acplug messages, which are wire-compatible with both old `HealthResponse`s (`status = 1`, `detail = 2`).
4. **Secrets cross one interface.** `host.SecretResolver.Resolve(ctx, ref)` in operator scope. `host.EnvSecrets` resolves `env:` and `file:`; ae's `tokenEnv: NAME` becomes `credentialRef: env:NAME`.

## Consequences

- An engine adopting plugkit deletes its `plugin/{dial,httpconn,config}.go` equivalents and keeps only its port adapters.
- The legacy hook is the whole cost of UP-O1; removing it after the window deletes one field from `host.Host` per engine.
- `serve` imports `host` only for `ProtocolID`; a plugin author in Go pulls the gRPC client too. Accepted: the constant would otherwise be duplicated.
