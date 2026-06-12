#!/usr/bin/env bash
# Day 44 — Break 02. Run blind. Do not read this file first.
set -euo pipefail

kubectl apply -f - >/dev/null <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: sneaky-default-deny
  namespace: podlab-prod
spec:
  podSelector: {}
  policyTypes:
    - Egress
EOF

echo "break-02 armed. Start your 20-minute timer."
echo "Symptom to investigate: production is misbehaving. Dev and stage are reportedly fine."
