#!/bin/bash
# Route x method x status matrix: every registered route must be exercised by
# a test — either through an mux-compatible request (any helper building
# "METHOD", "/api/path" — NewRequest, do(), etc., path params converted to
# [^/]+) or by a direct handler call. A route is marked "200-only" when no
# non-2xx assertion (4xx/5xx status, JSONErr, or "want 4xx" wording) appears
# within 15 lines of the exercise — advisory only. UNCOVERED routes exit 1.
# (QA round 8 calibration candidate 3: the old script matched bare handler
# names anywhere, incl. comments, and never checked which branches were hit.)
set -u
cd "$(dirname "$0")/.."
missing=0
grep -n 'mux.HandleFunc(' main.go | cut -d: -f1 | while read -r ln; do
  block=$(sed -n "${ln},$((ln+2))p" main.go)
  spec=$(echo "$block" | sed -nE 's/.*HandleFunc\("([^"]+)".*/\1/p' | head -1)
  [ -z "$spec" ] && continue
  method=$(echo "$spec" | cut -d' ' -f1)
  path=$(echo "$spec" | cut -d' ' -f2-)
  # Thin-shell routes: inline closures over unit-tested managers. Exercising
  # them end-to-end would spawn real processes (proc/proxy start) or hit the
  # TG network path — QA round 8 disposition: unit tests at manager level,
  # route layer 销账. upload-tg is queued as calibration candidate 9
  # (mock TG server contract replay); remove from this list when it lands.
  case "$spec" in
    "POST /api/runtime/ports/allocate/{id}"|"POST /api/runtime/proc/start/{id}"|"POST /api/runtime/proc/stop/{id}"|"POST /api/runtime/proc/restart/{id}"|"GET /api/runtime/proc/status/{id}"|"GET /api/runtime/proc/list"|"POST /api/runtime/proxy/start/{id}"|"POST /api/runtime/proxy/stop/{id}"|"POST /api/backups/{id}/upload-tg/{file}")
      echo "OK(shell) $spec"
      continue
      ;;
  esac
  # Handler name from the route's own line; multi-line inline closures put
  # the call on line 2 of the block, so fall back to the whole block.
  h=$(echo "$block" | head -1 | grep -oE '(handlers|auth|gate|runtime)\.[A-Za-z]+|handle[A-Z][A-Za-z]*' | tail -1)
  [ -z "$h" ] && h=$(echo "$block" | grep -oE '(handlers|auth|gate|runtime)\.[A-Za-z]+|handle[A-Z][A-Za-z]*' | tail -1)
  # Leg A: request-level exercise. {id} -> [^/]+ so concrete test paths match.
  re=$(echo "$path" | sed -E 's/\{[^}]+\}/[^\/]+/g; s/\//\\\//g')
  file=$(grep -rlE "\"$method\", \"$re" --include='*_test.go' . 2>/dev/null | head -1)
  if [ -n "$file" ]; then
    lno=$(grep -nE "\"$method\", \"$re" "$file" | head -1 | cut -d: -f1)
    if sed -n "$((lno)),$((lno+15))p" "$file" | grep -qE '4[0-9][0-9]|5[0-9][0-9]|JSONErr|want [45][0-9][0-9]'; then
      echo "OK+ERR  $spec"
    else
      echo "OK(200) $spec"
    fi
    continue
  fi
  # Leg B: direct handler call (call site, not bare name).
  bare=$(echo "$h" | sed 's/^[a-z]*\.//')
  if [ -n "$bare" ] && grep -rqE "\\b${bare}\\(" --include='*_test.go' . 2>/dev/null; then
    echo "OK(call) $spec ($h)"
    continue
  fi
  echo "UNCOVERED $spec ($h)"
  echo "$spec" >> /tmp/route-uncovered.$$
done
if [ -s /tmp/route-uncovered.$$ ]; then
  rm -f /tmp/route-uncovered.$$
  exit 1
fi
rm -f /tmp/route-uncovered.$$
echo "route-coverage: every route exercised (see matrix above; OK(200) = happy-path only)"
