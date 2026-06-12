#!/usr/bin/env bash
# Day 44 — Break 01. Run blind. Do not read this file first.
set -euo pipefail

mkdir -p /tmp/day44
kubectl -n kube-system get deployment coredns -o jsonpath='{.spec.replicas}' > /tmp/day44/break-01-replicas 2>/dev/null || echo 2 > /tmp/day44/break-01-replicas

kubectl -n kube-system scale deployment coredns --replicas=0 >/dev/null

echo "break-01 armed. The cluster is now subtly broken."
echo "Start your 20-minute timer. Symptom to investigate: things stopped talking to each other."
