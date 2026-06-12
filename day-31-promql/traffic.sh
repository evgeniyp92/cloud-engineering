#!/usr/bin/env bash
# Steady traffic generator for podlab. Run in a spare terminal; Ctrl-C stops it.
#
# Usage:
#   ./traffic.sh                                  # clean traffic to prod
#   HOST=podlab-stage.localhost ./traffic.sh      # different environment
#   ERROR_RATE=0.3 ./traffic.sh                   # also hit /error?rate=0.3
#
# HOST must match the Ingress host of the overlay you target:
#   kubectl get ingress -A   # check your dev/stage/prod hosts
set -u

HOST="${HOST:-podlab-prod.localhost}"
PORT="${PORT:-8080}"
ERROR_RATE="${ERROR_RATE:-0}"
URL="http://${HOST}:${PORT}"

echo "traffic -> ${URL}   (ERROR_RATE=${ERROR_RATE})"
echo "Ctrl-C to stop"

i=0
while true; do
  curl -s -o /dev/null --max-time 2 "${URL}/"
  curl -s -o /dev/null --max-time 2 "${URL}/config"
  if [ "${ERROR_RATE}" != "0" ]; then
    curl -s -o /dev/null --max-time 2 "${URL}/error?rate=${ERROR_RATE}"
  fi
  i=$((i + 1))
  if [ $((i % 50)) -eq 0 ]; then echo "  ...${i} loops"; fi
  sleep 0.2
done
