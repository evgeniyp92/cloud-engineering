#!/usr/bin/env bash
# Day 48 reference: bootstrap.sh — empty kind cluster → full platform.
# Copy into ~/Code/k8s-gitops (the script CAN live in git; the key backup CANNOT).
# Adjust versions/paths to match your repo before running.
set -euo pipefail

CLUSTER_NAME="course"
KIND_CONFIG="${KIND_CONFIG:-$HOME/Code/cloud-engineer-course/day-15-network-policies/kind-config-cilium.yaml}"
KEY_BACKUP="${KEY_BACKUP:-$HOME/sealed-secrets-key-backup.yaml}"   # OUTSIDE git. Non-negotiable.
CILIUM_VERSION="${CILIUM_VERSION:-1.16.5}"                          # pin to what you ran on Day 15

# ── 0. pre-flight ────────────────────────────────────────────────────────────
[ -f "$KEY_BACKUP" ] || { echo "FATAL: sealed-secrets key backup not found at $KEY_BACKUP"; exit 1; }
command -v kind >/dev/null && command -v helm >/dev/null && command -v kubectl >/dev/null

# ── 1. cluster ───────────────────────────────────────────────────────────────
kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CONFIG"

# ── 2. CNI — nothing schedules without it ───────────────────────────────────
helm repo add cilium https://helm.cilium.io --force-update
helm install cilium cilium/cilium --version "$CILIUM_VERSION" \
  --namespace kube-system \
  --set image.pullPolicy=IfNotPresent \
  --set ipam.mode=kubernetes
kubectl wait --for=condition=Ready nodes --all --timeout=300s

# ── 3. sealed-secrets key — BEFORE the controller ever starts ────────────────
# The controller adopts existing keys labeled active on startup. Restore the
# old key now and every SealedSecret in git decrypts; restore it later and the
# controller has already minted a fresh key your secrets weren't sealed with.
kubectl create namespace sealed-secrets --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "$KEY_BACKUP"   # the backup carries its own ns/labels — verify they match your install ns!

# ── 4. ArgoCD — the imperative seed ──────────────────────────────────────────
kubectl create namespace argocd --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml
kubectl wait --for=condition=Available deploy/argocd-server -n argocd --timeout=300s

# ── 5. the root app — git takes over from here ───────────────────────────────
kubectl apply -f "$(dirname "$0")/argocd/root.yaml"

echo ""
echo "Bootstrap seeded. ArgoCD is reconciling the platform from git."
echo "Watch:  kubectl get applications -n argocd -w"
echo "UI:     kubectl port-forward svc/argocd-server -n argocd 8083:443"
