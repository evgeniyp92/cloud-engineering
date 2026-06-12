#!/usr/bin/env bash
# Day 39 traffic generator for the canary lab.
#
#   ./traffic.sh        # good traffic: GET / in a loop
#   ./traffic.sh bad    # degraded: every 3rd request hits /error?rate=0.8
#                       # → ~27% of all requests return 500 → success rate ~0.73
#
# Prints one line per request and a rolling tally every 25 requests.
# Ctrl-C to stop.
set -u

HOST="${HOST:-http://canary.localhost:8080}"
MODE="${1:-good}"

i=0; errors=0
declare -A versions

while true; do
  i=$((i + 1))

  if [ "$MODE" = "bad" ] && [ $((i % 3)) -eq 0 ]; then
    code=$(curl -s -o /dev/null -m 2 -w '%{http_code}' "$HOST/error?rate=0.8" || echo 000)
    [ "$code" != "200" ] && errors=$((errors + 1))
    printf '%4d  /error  %s\n' "$i" "$code"
  else
    body=$(curl -s -m 2 "$HOST/" || true)
    code=200
    case "$body" in
      *'"version"'*) : ;;
      *) code=000; errors=$((errors + 1)) ;;
    esac
    ver=$(printf '%s' "$body" | sed -nE 's/.*"version":"([^"]*)".*/\1/p')
    col=$(printf '%s' "$body" | sed -nE 's/.*"color":"([^"]*)".*/\1/p')
    key="${ver:-?}/${col:-?}"
    versions[$key]=$(( ${versions[$key]:-0} + 1 ))
    printf '%4d  /       %s  %s\n' "$i" "$code" "$key"
  fi

  if [ $((i % 25)) -eq 0 ]; then
    tally=""
    for k in "${!versions[@]}"; do tally+="$k=${versions[$k]} "; done
    echo "---- after $i requests: $tally errors=$errors ----"
  fi

  sleep 0.2
done
