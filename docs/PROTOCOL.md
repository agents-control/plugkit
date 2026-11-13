# acplug/1 wire rules

## Handshake

1. The host dials (TLS by default; plaintext only with `tls.insecure: true`) and calls `acplug.v1.Plugin/Manifest` with `host_protocols`, `host_engine`, `host_version`, within 10 s.
2. The host refuses the plugin when: `protocol` is not one of `host_protocols` (or an accepted legacy protocol); `name` or `version` is empty; `protocol_version` is not a valid range containing 1; no port is declared; a port has no name or no `config_schema` (a JSON Schema object, or `true`); a configured port is not declared; `require.version` (semver terms such as `>=0.3.0 <1.0.0`) does not hold; a `require.capabilities` entry is missing after narrowing; a port's config does not validate against its schema.
3. `Health` must answer `STATUS_SERVING`.
4. The host pins `sha256` of the deterministic manifest encoding. A later different digest marks the plugin `degraded`.

Capabilities are `name` or `name=value`. The host keeps those whose name its build supports for that port.

## Errors

Domain failures are typed fields of each port's reply. gRPC `UNAVAILABLE`, `DEADLINE_EXCEEDED`, `RESOURCE_EXHAUSTED`, `ABORTED` and `CANCELLED` mean the plugin could not serve the call; the caller may retry. A policy, admission or licence plugin error is a denial. Numbers above 2^53 travel as strings; the host refuses to send a number a double would round.

## HTTP/JSON mapping

For call-out ports, and unary methods of any port:

- `POST {endpoint}/<package>.<Service>/<Method>`, body and reply are protobuf JSON with field names as in the `.proto` (`host_protocols`, `config_schema`), enums by name. Unknown reply fields are ignored.
- Headers: `Content-Type: application/json`, `Authorization: Bearer <token>` when a credential is configured, `Acplug-Deadline-Ms: <remaining ms>`.
- Server streams answer `application/x-ndjson`, one message per line; blank lines are keep-alives.
- Errors: a non-2xx status with `{"error": {"code": "<gRPC code name>", "message": "…"}}`, or the same object as a stream line. Without a body the status maps 400→INVALID_ARGUMENT, 401→UNAUTHENTICATED, 403→PERMISSION_DENIED, 404→NOT_FOUND, 429→RESOURCE_EXHAUSTED, 501→UNIMPLEMENTED, 502/503→UNAVAILABLE, 504→DEADLINE_EXCEEDED. A method a plugin does not serve answers `UNIMPLEMENTED`.
- Redirects are not followed. Reply bodies above the message size limit fail with `RESOURCE_EXHAUSTED`.

## Limits

`ProvidedPort.limits` lowers the host's ceilings for that port: `deadline_ms` (unary calls), `max_in_flight` (excess calls fail fast with `RESOURCE_EXHAUSTED`), `max_message_bytes` (both directions).

## EventSink

`Publish` receives a batch at least once. `accepted` must equal the batch size, otherwise the whole batch is redelivered. Deduplicate by `Event.id`. Engine-specific attributes (run id, sequence, epoch) arrive in `extensions`.
