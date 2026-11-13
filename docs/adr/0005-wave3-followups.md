# ADR-0005: engine follow-ups of universal-plug waves 2 and 3

Status: **Accepted** (2026-11-13). Refines UP-ADR-001 §3, §6 and UP-ADR-002 §2.

## Decision

| # | Change | Why |
|---|---|---|
| 1 | `conform.Result` gains `skipped`, `reason`, `optional`; `conform.Skip(reason)`; `Case.Optional`; `Report.Skipped()`. `Failed()` lists failures and required skips, so `Report.Passed` and `VerifyReport` refuse a required skip | A case that cannot apply must not read as a pass; a suite says which cases may be skipped |
| 2 | `Spec.Validate func(File) Findings` runs at the end of `Check` | The engine runs the same `Check` at boot, so `adapters check` refuses exactly what boot refuses (preflight equals boot) |
| 3 | `serve.Manifest(protocol, name, version, ports...)` and `serve.Port(name, caps...)`; `SinkManifest` is built from them | wf wrote its own `remote.Manifest`; one builder for every engine |
| 4 | `ManifestRequest.port_config` (field 4, `map<string, Struct>`) carries each enabled port's `config` | Plugins read settings from the handshake, not side env vars. The host still validates the config against `config_schema` after the reply, so a plugin must validate its own copy before trusting it. Config holds references only (ADR-0003 §6), never secret values |
| 5 | `ProvidedPort.protocols` (field 7): protocol ids the port speaks, empty means the manifest protocol. A port is enabled only if the host speaks one of them; naming such a port in `ports` is an error | One plugin binary can serve ports of several engine families, for example `acplug/1+mtplugin/1` and `acplug/1+artplugin/1`, each host enabling its own |
| 6 | `Suite.Cases(ctx, conn)` takes the run context; the fixed 30 s per-case timeout is removed; `Case.Timeout` narrows it; `conform -timeout` bounds a CLI run and Ctrl-C cancels it | Deadlines and cancellation belong to the runner. This is a Go API break for engines that build suites (add a `context.Context` first parameter); the wire contract only grows |
| 7 | `acknowledge: [name]` in the file and `<P>_ACKNOWLEDGE` in env; `Spec.Acknowledgements` lists the names an engine defines, `File.Acknowledged(name)` reads them; an unknown name is a `Check` error. With a file, env must agree with it, like `allowMemory` | Engines require an explicit opt-in (for example `partial_store_plugin`) from the file instead of ad-hoc env vars, and the opt-in shows in review |

Review fixes: acknowledgements appear in `Document.Acknowledge` and in `Diff` (port `acknowledge`, weakening per added name), and the env-only `<P>_ACKNOWLEDGE` form is refused in production; per-port protocols match only the host's own ids (legacy ids only for a legacy plugin), the host picks the first of its protocols the port offers and exposes it as `Client.Protocol(port)` and the discovery row `protocol`; a manifest with no speaking port or a repeated port is refused; a non-object port config is refused; a skip counts only as the sole error; the corpus run uses the same timeout; `adapters.Dial(ctx, host, spec, file, suites...)` runs `Check` itself.

`buf breaking` passes against the previous baseline; the baseline is refreshed with the two new fields.
