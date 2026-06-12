#!/usr/bin/env bash
# Day 44 — undo every break script. Safe to run repeatedly, in any state.
set -uo pipefail

echo "== break-01: CoreDNS =="
REPLICAS=$(cat /tmp/day44/break-01-replicas 2>/dev/null || echo 2)
kubectl -n kube-system scale deployment coredns --replicas="$REPLICAS" 2>/dev/null && echo "  coredns → $REPLICAS replicas"

echo "== break-02: NetworkPolicy =="
kubectl delete networkpolicy sneaky-default-deny -n podlab-prod --ignore-not-found

echo "== break-03: ArgoCD RBAC =="
if kubectl get clusterrolebinding argocd-application-controller >/dev/null 2>&1; then
  echo "  ClusterRoleBinding already present"
elif [ -f /tmp/day44/break-03-crb.yaml ]; then
  kubectl apply -f /tmp/day44/break-03-crb.yaml && echo "  ClusterRoleBinding restored from backup"
else
  echo "  WARNING: no backup at /tmp/day44/break-03-crb.yaml — re-apply the ArgoCD install manifest:"
  echo "  kubectl apply -n argocd -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml"
fi

echo "== break-04: node taints =="
kubectl taint nodes course-worker  maintenance:NoSchedule- 2>/dev/null || echo "  course-worker: no taint to remove"
kubectl taint nodes course-worker2 maintenance:NoSchedule- 2>/dev/null || echo "  course-worker2: no taint to remove"

echo "== break-05: ingress + TLS =="
REPLICAS=$(cat /tmp/day44/break-05-replicas 2>/dev/null || echo 1)
kubectl -n ingress-nginx scale deployment ingress-nginx-controller --replicas="$REPLICAS" 2>/dev/null && echo "  ingress-nginx-controller → $REPLICAS replicas"
# cert-manager normally re-issues podlab-tls on its own; restore from backup only if it hasn't
if ! kubectl get secret podlab-tls -n rollouts-lab >/dev/null 2>&1 && [ -f /tmp/day44/break-05-secret.yaml ]; then
  kubectl apply -f /tmp/day44/break-05-secret.yaml && echo "  podlab-tls restored from backup"
fi

echo
echo "== health sweep =="
kubectl get --raw /readyz >/dev/null 2>&1 && echo "  apiserver: ok"
kubectl -n kube-system get deploy coredns -o jsonpath='  coredns ready: {.status.readyReplicas}{"\n"}'
kubectl get nodes --no-headers | awk '{print "  " $1 ": " $2}'
echo "Done. Give controllers ~1 min to settle, then re-run your verify checks."
