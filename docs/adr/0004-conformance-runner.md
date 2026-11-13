# ADR-0004: one conformance runner, suites registered per port, a corpus that must fail

Status: **Accepted** (2026-11-13). Implements UP-ADR-001 §6.

## Decision

1. **Runner.** `conform.Run(ctx, host, config, suites...)` dials through the real host, so the handshake rules are part of conformance. Every plugin runs the protocol cases `handshake`, `manifest is stable` and `health answers` (20 Health round trips, p50/p99 in the report). Then each enabled port runs its registered `conform.Suite`; **a port without a suite fails** (`suite available`), because nothing proves it.
2. **Report.** `acplug.conform/1` JSON: plugin, version, protocol, manifest digest, suite versions, cases with `passed`, `error` and milliseconds, latency, `passed`. Its digest is sha256 over the compact JSON encoding; `VerifyReport` re-derives it from the file.
3. **Usable without Go.** `cmd/conform` is the binary plugkit ships, with the suites plugkit owns (`sinktest`). Engines embed the same `conform.CLI` with their own suites (`wfcore conform …`). Flags cover gRPC or HTTP, TLS/mTLS, a credential reference, the ports to check and their config.
4. **Corpus.** `conform corpus` serves every fixture of `conform/corpus` on loopback and requires `Verify` to hold: the reference passes and each broken plugin fails at the named case (protocol range, unknown protocol, port without schema, not serving, manifest drift, partial sink acceptance, refused redelivery, ignored deadline). Engines append their own fixtures (store without outbox, cross-tenant record, journal overwrite, blob digest mismatch) next to their suites.
5. **Go suites run on Go adapters too.** A suite is a list of `Case{Name, Run}` built over a port value (`sinktest.Cases(outbox.Sink)`), so the same cases check the memory fake, the built-in and a plugin: the drift guard of UP-ADR-001 §6.1.
6. **Wave-3 additions** (see ADR-0005). A case may return `conform.Skip(reason)`: the result is `skipped`, not `passed`, and counts as a failure (in `Failed`, `Passed`, `VerifyReport`) unless the case is `Optional`. `Suite.Cases` receives the run context; the fixed 30 s case timeout is gone, the caller's context (`conform -timeout`, 5 min by default) and the optional `Case.Timeout` bound the run.
