# plugkit

The shared plug layer of the agents.control engines. Every engine port can be served by a built-in adapter, an in-memory fake or **anyone's plugin in any language**, selected by configuration alone. plugkit holds the part that is the same for every engine: the `acplug/1` protocol, the host that dials and guards plugins, the `adapters.yaml` desired state with its check and plan, the `GET /v1/adapters` document and the conformance runner. Engines keep their own ports, domain services, suites and built-ins. plugkit imports no engine.

## Packages

| Path | Owns |
|---|---|
| `api/acplug/v1` | `Plugin` (Manifest, Health), `Scope`, `Limits`, `EventSink`; buf lint + breaking against `api/baseline/acplug.binpb` |
| `host` | dial over gRPC or HTTP/JSON, TLS/mTLS, bearer from a secret reference, handshake, manifest pinning and drift, config-schema checks, capability narrowing, per-port deadline/bulkhead/size guards, legacy manifests |
| `adapters` | `adapters.yaml` + env form + aliases, `Check`, `Diff` (`weakening`, `data_migration_required`), `Dial` with conformance verification, `Describe`, `adapters check|plan` CLI |
| `discovery` | the `GET /v1/adapters` document, handler and fetch |
| `conform` | runner, `acplug.conform/1` report, `Verify`, embeddable CLI; `conform/corpus` broken plugins |
| `outbox`, `outbox/sinktest` | batch `Sink` port, its plugin adapter, the Standard Webhooks `WebhookSink`, `ParseKey`/`Verify`, and the suite |
| `usage` | `Annotate`: the neutral `x-usage` annotations of an event schema (MT-ADR-010) from `usage:"…"` struct tags, for every engine's schema generator |
| `serve` | helpers to write a Go plugin |
| `cmd/conform` | the conformance binary: no Go needed on the plugin side |
| `examples/python-sink` | a stdlib Python sink plugin that passes `conform` |

Decisions: `docs/adr/`. Wire rules: `docs/PROTOCOL.md`. Cross-engine design: UP-ADR-001 and UP-ADR-002 in the research repository.

## Prove a plugin

```
make conform
python3 examples/python-sink/plugin.py --port 8099 &
bin/conform -endpoint http://127.0.0.1:8099 -protocol http -insecure -report report.json
bin/conform corpus
```

`conform` exits 0 only if every case passes. Put the printed report digest and the report path in `require.conformance` and `require.report` of `adapters.yaml`; production refuses a plugin without them.

## Select adapters

```yaml
apiVersion: acplug/v1
kind: Adapters
engine: workflow-engine
mode: production
ports:
  sink: {use: plugin:py-sink}
plugins:
  - name: py-sink
    endpoint: https://py-sink:8443
    protocol: http
    tls: {ca: /etc/wf/ca.pem}
    credentialRef: env:WF_SINK_TOKEN
    require: {conformance: "sha256:…", report: /etc/wf/conform/py-sink.json}
```

Engines may declare extra sections under `extensions: {<key>: …}` (ADR-0007): schema-checked, secrets as references only, shown in `GET /v1/adapters` only through the engine's `Discover` projection, weakening changes flagged by `plan`.

Each engine exposes `<engine> adapters check --file f` and `<engine> adapters plan --file f --against http://engine:8080`.

## Develop

```
make check   gofmt, vet, staticcheck, comment gate, dependency rule, buf lint, buf breaking
make test    go test -race (the example test needs python3)
make proto   regenerate api/acplug/v1
```

MIT licensed.
