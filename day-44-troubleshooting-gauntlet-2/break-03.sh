#!/usr/bin/env bash
# Day 44 — Break 03. Run blind. Do not read this file first.
set -euo pipefail

CRB="argocd-application-controller"
mkdir -p /tmp/day44

if ! kubectl get clusterrolebinding "$CRB" >/dev/null 2>&1; then
  echo "ERROR: ClusterRoleBinding '$CRB' not found — your ArgoCD install names it differently." >&2
  echo "Find it with: kubectl get clusterrolebinding | grep argocd  — then edit CRB= in this script." >&2
  exit 1
fi

kubectl get clusterrolebinding "$CRB" -o yaml > /tmp/day44/break-03-crb.yaml
kubectl delete clusterrolebinding "$CRB" >/dev/null
# bounce the controller so cached watches don't mask the damage
kubectl -n argocd delete pod -l app.kubernetes.io/name=argocd-application-controller >/dev/null 2>&1 || true

echo "break-03 armed. Start your 20-minute timer."
echo "Symptom to investigate: GitOps has quietly stopped doing its one job."
