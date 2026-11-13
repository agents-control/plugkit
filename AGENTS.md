# Instructions for agents working on plugkit

Read `README.md`, `docs/PROTOCOL.md` and `docs/adr/` before changing code. The ADRs are decisions, not suggestions. The cross-engine design is UP-ADR-001 and UP-ADR-002 (agents-control-research, `state/specs/universal-plug/`).

## Rules

- **No engine imports.** plugkit never imports workflow-engine, artifact-engine, test-engine or metering-engine; engines import plugkit. Engine-specific behaviour enters through `adapters.Spec`, `host.Host` (protocols, deadlines, supported capabilities, legacy hooks) and registered `conform.Suite`s.
- **Dependency rule.** `scripts/check-deps.sh` holds every allowed edge with its reason; add an edge only with a reason.
- **Contract.** `api/acplug/v1` is public. `make check` runs `buf lint` and `buf breaking` against `api/baseline/acplug.binpb`; refresh the baseline (`make api-baseline`) only on a deliberate, versioned change.
- **Zero inline comments.** Godoc on exported symbols only, at most 2 lines, kept current (`scripts/check-comments.sh`).
- **Errors** are wrapped with `%w`; plugin failures surface as gRPC status codes classified by `host.Unavailable`; secrets are references, never values, never logged; compare tokens in constant time.
- **Tests** use `-race` and assert observable behaviour. Every broken-plugin class gets a fixture in `conform/corpus` that the runner must refuse.
- **No duplicated code, no useless wrappers, no leaked abstractions.**
- **Done means** `make check` and `make test` green.

## Commands

```
make check         gofmt, vet, staticcheck, comments, dependency rule, buf lint + breaking
make test          go test -race -count=1 ./...
make proto         regenerate api/acplug/v1
make conform       build bin/conform
```
