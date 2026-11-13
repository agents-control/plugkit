# ADR-0002: every plugin call goes through a per-port guard: deadline, bulkhead, size

Status: **Accepted** (2026-11-13). Refines UP-ADR-001 §3.5 and §3.6.

## Decision

`Client.Port(name)` returns a `grpc.ClientConnInterface` that wraps the plugin connection (gRPC or HTTP/JSON alike) with three guards, built once per enabled port at the handshake:

| Guard | Value | On breach |
|---|---|---|
| Deadline (unary) | `min(Host.Deadlines[port] or Host.DefaultDeadline or 10 s, Limits.deadline_ms, caller context)` | `DEADLINE_EXCEEDED` |
| Bulkhead | `min(Host.MaxInFlight or 64, Limits.max_in_flight)` slots, **fail fast** | `RESOURCE_EXHAUSTED` without calling the plugin |
| Message size | `min(Host.MaxMessageBytes or 4 MiB, Limits.max_message_bytes)`, both directions | `RESOURCE_EXHAUSTED` before sending; gRPC enforces the receive side, the HTTP mapping caps the body |

- **Streams have their own pool and an idle timeout, not a deadline** (wave 2). A stream holds one slot of its port's stream pool (`Host.StreamCaps[port]` or `Host.DefaultStreamCap`, 1024 by default), never a unary slot, from open until it ends (terminal receive, idle timeout or caller cancellation). Every `SendMsg` and `RecvMsg` must complete within the idle timeout, `Host.StreamIdle[port]` or `Host.DefaultStreamIdle` (60 s by default, negative disables), otherwise the stream is cancelled with `DEADLINE_EXCEEDED`. A plugin that accepts a stream and never answers can therefore neither hang the caller nor exhaust the host, and long-lived streams (one trigger `Watch` per binding) cannot starve unary calls. Per-tenant stream caps belong to the engine, which knows its tenants; the host cap only protects the process. Ports with legitimately silent streams (a trigger waiting for events) raise or disable their idle timeout; the caller's context still bounds the whole stream, and a bounded session (UP-ADR-001 §4.3, 2 s) sets that context itself.
- **Fail fast over queueing.** A full bulkhead is a retryable unavailability (`host.Unavailable`), so the engine's existing retry and breaker decorators decide what to do; a hidden queue would hold the caller past its own deadline.
- **The HTTP mapping carries the budget.** Every request has `Acplug-Deadline-Ms` with the remaining milliseconds, so a stdlib plugin can honour it; gRPC already sends `grpc-timeout`.
- **Plaintext needs `tls.insecure: true`, always.** A loopback or Unix endpoint does not downgrade on its own. `Config.Plaintext()` is true only for insecure traffic to a non-loopback endpoint, and only that case is a `weakening` flag and a check warning.
- **Pinning.** The handshake records `{name, version, protocol, sha256 of the deterministic manifest encoding}`. `Client.Recheck` re-reads Health and the manifest; a different digest sets `degraded` and keeps the pinned contract until a new plan is applied. Legacy plugins are pinned but not re-read (their manifest needs the engine hook).
