#!/usr/bin/env bash
# Day 44 — Break 04. Run blind. Do not read this file first.
set -euo pipefail

kubectl taint nodes course-worker  maintenance=true:NoSchedule --overwrite >/dev/null
kubectl taint nodes course-worker2 maintenance=true:NoSchedule --overwrite >/dev/null

# surface the symptom: force some pods to need rescheduling
kubectl -n podlab-dev rollout restart deployment >/dev/null 2>&1 || true

echo "break-04 armed. Start your 20-minute timer."
echo "Symptom to investigate: everything that was running still runs… but nothing new arrives."
