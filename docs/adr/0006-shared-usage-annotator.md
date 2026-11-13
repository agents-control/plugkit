# ADR-0006: one x-usage annotator for every engine

Status: **Accepted** (2026-11-13). Applies MT-ADR-010 §2 and PL-ADR-001 R10.

## Context

Every engine generates the JSON Schema of its event payloads with `invopop/jsonschema` and annotates billable facts with `x-usage` from struct tags. artifact-engine wrote the tag reader in `cmd/schemagen`, workflow-engine its own, and test-engine v2 needed a third copy.

## Decision

`plugkit/usage` holds the annotator, taken from artifact-engine's (the richest): `Event` (root `count`, `stock`, `exclude`, `StockDimensions`), `Stock`, `Exclude`, and `Annotate(schema, payload type, root, roles)`. Tags: `quantity=<unit>`, `quantities` (a map of quantities), `dimension=<name>` (or `*`), `actor=<role>`, `actorFrom=<path>` (the engine passes its kind-to-role map). Quantities are written only when the event counts; dimensions only when it counts or sets `StockDimensions`; attribution always. Embedded structs are walked; slices annotate their items.

## Consequences

- plugkit depends on `invopop/jsonschema` (already in every engine).
- test-engine adopts it first; artifact-engine and workflow-engine replace their copies when they next touch their generators.
