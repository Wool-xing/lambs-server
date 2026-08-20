#!/bin/bash
# Route→test coverage diff: every registered route's handler must appear in
# some _test.go file (direct handler call or E2E through newMux). Unknown
# routes are printed as UNCOVERED; exit 1 if any. Route discovery greps
# main.go + 2 lines of context so multi-line inline handlers are seen.
set -u
cd "$(dirname "$0")/.."
missing=0
grep -n 'mux.HandleFunc' main.go | cut -d: -f1 | while read -r ln; do
  block=$(sed -n "${ln},$((ln+2))p" main.go)
  route=$(echo "$block" | sed -nE 's/.*HandleFunc\("([A-Z]+ [^"]+)".*/\1/p' | head -1)
  first=$(echo "$block" | head -1)
  h=$(echo "$first" | grep -oE '(handlers|auth|gate)\.[A-Za-z]+|handle[A-Z][A-Za-z]*' | tail -1)
  [ -z "$route" ] && continue
  # Same-package tests call handlers without the package prefix.
  bare=$(echo "$h" | sed 's/^[a-z]*\.//')
  if [ -n "$bare" ] && grep -rqE "\\b${bare}\\b" --include='*_test.go' .; then
    continue
  fi
  echo "UNCOVERED: $route ($h)"
  echo "$route" >> /tmp/route-uncovered.$$
done
if [ -s /tmp/route-uncovered.$$ ]; then
  rm -f /tmp/route-uncovered.$$
  exit 1
fi
rm -f /tmp/route-uncovered.$$
echo "route-coverage: all routes referenced in tests"
