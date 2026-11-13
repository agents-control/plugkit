# ADR-0002: every plugin call goes through a per-port guard: deadline, bulkhead, size

Status: **Accepted** (2026-11-13). Refines UP-ADR-001 §3.5 and §3.6.

## Decision

`Client.Port(name)` returns a `grpc.ClientConnInterface` that wraps the plugin connection (gRPC or HTTP/JSON alike) with three guards, built once per enabled port at the handshake:

| Guard | Value | On breach |
|---|---|---|
| Deadline (unary) | `min(Host.Deadlines[port] or Host.DefaultDeadline or 10 s, Limits.deadline_ms, caller context)` | `DEADLINE_EXCEEDED` |
| Bulkhead | `min(Host.MaxInFlight or 64, Limits.max_in_flight)` slots, **fail fast** | `RESOURCE_EXHAUSTED` without calling the plugin |
| Message size | `min(Host.MaxMessageBytes or 4 MiB, Limits.max_message_bytes)`, both directions | `RESOURCE_EXHAUSTED` before sending; gRPC enforces the receive side, the HTTP mapping caps the body |

- **Streams get size limits only.** A server stream (trigger `Watch`, journal `Claim`) lives as long as its caller wants, so its deadline is the caller's context. A bounded session (UP-ADR-001 §4.3, 2 s) sets that context itself.
- **Fail fast over queueing.** A full bulkhead is a retryable unavailability (`host.Unavailable`), so the engine's existing retry and breaker decorators decide what to do; a hidden queue would hold the caller past its own deadline.
- **The HTTP mapping carries the budget.** Every request has `Acplug-Deadline-Ms` with the remaining milliseconds, so a stdlib plugin can honour it; gRPC already sends `grpc-timeout`.
- **Plaintext needs `tls.insecure: true`, always.** A loopback or Unix endpoint does not downgrade on its own. `Config.Plaintext()` is true only for insecure traffic to a non-loopback endpoint, and only that case is a `weakening` flag and a check warning.
- **Pinning.** The handshake records `{name, version, protocol, sha256 of the deterministic manifest encoding}`. `Client.Recheck` re-reads Health and the manifest; a different digest sets `degraded` and keeps the pinned contract until a new plan is applied. Legacy plugins are pinned but not re-read (their manifest needs the engine hook).
