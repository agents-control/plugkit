#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
files=$(find . -name '*.go' -not -path '*/node_modules/*' -not -name '*.pb.go' -not -name '*.gen.go' -not -name '*_gen.go' | sort)
fail=0
for f in $files; do
  if head -5 "$f" | grep -q 'Code generated'; then continue; fi
  out=$(awk -v file="$f" '
    BEGIN { raw = 0 }
    function flush() { if (run > 2) printf "%s:%d: comment block of %d lines (max 2)\n", file, start, run; run = 0 }
    {
      if (raw) { if (gsub(/`/, "`") % 2 == 1) raw = 0; next }
      t = $0; sub(/^[ \t]+/, "", t)
      if (t ~ /^\/\/(go:|line |nolint|lint:|export |\+build)/) { flush(); next }
      if (t ~ /^\/\//) { if (run == 0) start = NR; run++; next }
      flush()
      if (gsub(/`/, "`") % 2 == 1) { raw = 1; next }
      s = $0
      gsub(/"([^"\\]|\\.)*"/, "\"\"", s)
      gsub(/`[^`]*`/, "``", s)
      gsub(/\x27([^\x27\\]|\\.)*\x27/, "\x27\x27", s)
      if (s ~ /[^ \t].*\/\//) printf "%s:%d: inline comment after code\n", file, NR
    }
    END { flush() }' "$f")
  if [ -n "$out" ]; then echo "$out" >&2; fail=1; fi
done
[ "$fail" = 0 ] || { echo "comment gate: godoc is at most 2 lines and there are no inline comments" >&2; exit 1; }
echo "comments ok"
