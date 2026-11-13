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
- Server streams answer `application/x-ndjson`, one message per line, each line at most the message size limit; blank lines are keep-alives.
- **Raw-body streams** for byte payloads (blob `Put`/`Open`):
  - A client-streaming method whose request message has exactly one singular `bytes` field is `PUT {endpoint}/<package>.<Service>/<Method>` with `Content-Type: application/octet-stream`. The first message's other fields travel in the `Acplug-Message` header as base64url (unpadded) protobuf JSON; the body is the concatenation of every message's bytes field. Later messages may carry bytes only. The reply is a JSON body as for unary calls.
  - A server-streaming method whose reply message has exactly one singular `bytes` field is `GET {endpoint}/<package>.<Service>/<Method>` with the request in `Acplug-Message`. The body is the raw bytes, split by the host into messages of 64 KiB; an optional `Acplug-Message` response header fills the other fields of the first message.
  - Bidirectional streams have no HTTP mapping.
- Errors: a non-2xx status with `{"error": {"code": "<gRPC code name>", "message": "…"}}`, or the same object as a stream line. Without a body the status maps 400→INVALID_ARGUMENT, 401→UNAUTHENTICATED, 403→PERMISSION_DENIED, 404→NOT_FOUND, 429→RESOURCE_EXHAUSTED, 501→UNIMPLEMENTED, 502/503→UNAVAILABLE, 504→DEADLINE_EXCEEDED. A method a plugin does not serve answers `UNIMPLEMENTED`.
- Redirects are not followed. Reply bodies above the message size limit fail with `RESOURCE_EXHAUSTED`.

## Limits

`ProvidedPort.limits` lowers the host's ceilings for that port: `deadline_ms` (unary calls), `max_in_flight` (calls and open streams; excess fails fast with `RESOURCE_EXHAUSTED`), `max_message_bytes` (both directions). Each stream send or receive must complete within the host's idle timeout for the port (60 s by default).

`ProvidedPort.metadata` carries descriptive strings such as `media_type` or `description`; capabilities stay for what the host can rely on.

## EventSink

`Publish` receives a batch at least once. `accepted` must equal the batch size, otherwise the whole batch is redelivered. Deduplicate by `Event.id`. Engine-specific attributes (run id, sequence, epoch) arrive in `extensions`.
