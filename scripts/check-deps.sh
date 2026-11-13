#!/usr/bin/env bash
# Source of truth for the dependency rule (docs/adr/0001). Every allowed cross-package edge carries a one-line reason.
# Imports inside the same top-level package are always allowed; stdlib is always allowed. plugkit never imports an engine.
# EDGES: from-top | to (module-relative path or prefix, "*" = any) | reason
# EXTERNAL: package path or prefix (every matching rule applies) | allowed external import prefixes ("*" = any) | reason
set -euo pipefail
cd "$(dirname "$0")/.."
mod=$(go list -m)
graph=$(go list -deps -json=ImportPath,Imports,Standard ./...)

grpc='google.golang.org/grpc google.golang.org/protobuf'

EDGES='
host|api|the host speaks the generated acplug contract
serve|api|Go plugins serve the generated acplug contract
serve|host|the reference manifest names the host protocol id
outbox|api|the plugin sink speaks acplug EventSink
outbox|host|event data uses the double-safe JSON values of the host
outbox|conform|sinktest registers the sink suite into the runner
conform|host|the runner dials plugins through the host
conform|api|the corpus serves broken acplug plugins
conform|serve|the corpus builds its fixtures from the Go plugin helpers
adapters|host|plugins of the adapter file are host configs
adapters|conform|dialing verifies conformance reports
adapters|discovery|plans diff against the discovery document and Describe renders it
cmd|*|thin mains
'

EXTERNAL='
api|*|generated protocol code
host|'"$grpc"' github.com/santhosh-tekuri/jsonschema golang.org/x/mod|gRPC client, JSON Schema checks of port config, semver constraints
serve|'"$grpc"'|gRPC server helpers
outbox|'"$grpc"'|plugin sink client
conform|'"$grpc"'|suites receive guarded gRPC connections
adapters|go.yaml.in/yaml|adapters.yaml decoding
discovery||the discovery document is stdlib only
cmd|*|mains
'

edges=$(jq -rs --arg mod "$mod" '. as $all
  | ([$all[] | select(.Standard != true) | {(.ImportPath): true}] | add) as $nonstd
  | $all[] | select(.ImportPath | startswith($mod)) | .ImportPath as $p
  | (.Imports // [])[] | select($nonstd[.] == true) | [$p, .] | @tsv' <<<"$graph")

fail=0
declare -A seen used
while IFS=$'\t' read -r pkg imp; do
  rel=${pkg#"$mod"/}
  top=${rel%%/*}
  key="$rel $imp"
  [ -n "${seen[$key]:-}" ] && continue
  seen[$key]=1
  if [[ "$imp" == "$mod"/* ]]; then
    irel=${imp#"$mod"/}
    [ "${irel%%/*}" = "$top" ] && continue
    ok=0
    while IFS='|' read -r from to _; do
      [ "$from" = "$top" ] || continue
      if [ "$to" = "*" ] || [ "$irel" = "$to" ] || [[ "$irel" == "$to"/* ]]; then ok=1; used["$from|$to"]=1; break; fi
    done <<<"$EDGES"
    [ "$ok" = 1 ] || { echo "dependency rule: $rel must not import $irel" >&2; fail=1; }
  else
    found=0
    ok=0
    while IFS='|' read -r ptop allowed _; do
      [ -n "$ptop" ] && { [ "$rel" = "$ptop" ] || [[ "$rel" == "$ptop"/* ]]; } || continue
      found=1
      [ "$allowed" = "*" ] && ok=1
      for a in $allowed; do [[ "$imp" == "$a"* ]] && ok=1; done
    done <<<"$EXTERNAL"
    if [ "$found" = 0 ]; then echo "dependency rule: no rule for package $rel" >&2; fail=1
    elif [ "$ok" = 0 ]; then echo "dependency rule: $rel must not import external $imp" >&2; fail=1; fi
  fi
done <<<"$edges"

while IFS='|' read -r from to _; do
  [ -z "$from" ] && continue
  [ -n "${used[$from|$to]:-}" ] || { echo "dependency rule: allowed edge $from -> $to is unused: remove it" >&2; fail=1; }
done <<<"$EDGES"

[ "$fail" = 0 ] || exit 1
echo "deps ok"
