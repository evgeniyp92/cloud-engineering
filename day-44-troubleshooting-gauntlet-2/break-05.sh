#!/usr/bin/env bash
# Day 44 — Break 05. Run blind. Do not read this file first.
set -euo pipefail

mkdir -p /tmp/day44

# two cuts, one symptom-cloud
kubectl get secret podlab-tls -n rollouts-lab -o yaml > /tmp/day44/break-05-secret.yaml 2>/dev/null || true
kubectl delete secret podlab-tls -n rollouts-lab >/dev/null 2>&1 || true

kubectl -n ingress-nginx get deployment ingress-nginx-controller -o jsonpath='{.spec.replicas}' \
  > /tmp/day44/break-05-replicas 2>/dev/null || echo 1 > /tmp/day44/break-05-replicas
kubectl -n ingress-nginx scale deployment ingress-nginx-controller --replicas=0 >/dev/null

echo "break-05 armed. Start your 20-minute timer."
echo "Symptom to investigate: every *.localhost URL you own just died. ALL of them."
