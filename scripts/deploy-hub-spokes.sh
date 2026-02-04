#!/usr/bin/env bash
set -euo pipefail

# Deploy a hub first, get its PodIP, then deploy spokes that peer to that hub.
# Uses PodIP (hostNetwork=false) so you don't need to know IPs beforehand.
#
# Usage:
#   ./scripts/deploy-hub-spokes.sh [namespace]
# Env overrides:
#   HUB_RELEASE   (default: evpn-hub)
#   SPOKE_RELEASE (default: evpn-spokes)
#   HUB_NODE      (optional; if set, hub will be pinned to that node)
#   HUB_ASN       (default: 65000)
#   SPOKE_ASN     (default: 65000)  # set different to do eBGP
#   IMAGE_TAG     (default: dev)
#
# Prereqs: kubectl, helm configured to the target cluster.

NS="${1:-evpnlab}"
HUB_RELEASE="${HUB_RELEASE:-evpn-hub}"
SPOKE_RELEASE="${SPOKE_RELEASE:-evpn-spokes}"
HUB_ASN="${HUB_ASN:-65000}"
SPOKE_ASN="${SPOKE_ASN:-65000}"
IMAGE_TAG="${IMAGE_TAG:-dev}"
HUB_REPLICAS="${HUB_REPLICAS:-1}"
SPOKE_REPLICAS="${SPOKE_REPLICAS:-4}"
DYNAMIC_PREFIX="${DYNAMIC_PREFIX:-10.244.0.0/16}"

CHART_DIR="$(cd "$(dirname "$0")/.." && pwd)/charts/evpn-agent"

kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS"

NODE_SELECTOR_ARG=()
if [[ -n "${HUB_NODE:-}" ]]; then
  echo "Pin hub to node $HUB_NODE"
  NODE_SELECTOR_ARG+=(--set nodeSelector."kubernetes\.io/hostname"="$HUB_NODE")
fi

echo "Deploying hub ($HUB_RELEASE)..."
  helm upgrade --install "$HUB_RELEASE" "$CHART_DIR" -n "$NS" \
    --set hostNetwork=false \
    --set gobgp.enabled=true \
    --set gobgp.asn="$HUB_ASN" \
    --set gobgp.dynamic.enabled=true \
    --set gobgp.dynamic.prefix="$DYNAMIC_PREFIX" \
    --set gobgp.dynamic.peerAs="$SPOKE_ASN" \
    --set gobgp.rr.enabled=true \
    --set gobgp.rr.client=true \
    --set agent.localInterface=eth0 \
    --set agent.gobgpAddress=127.0.0.1:50051 \
    --set agent.advertiseSelf=true \
    --set image.tag="$IMAGE_TAG" \
    --set replicas="$HUB_REPLICAS" \
  "${NODE_SELECTOR_ARG[@]}" \
  --wait

echo "Waiting for hub Pod IP..."
for i in {1..30}; do
  HUB_IP="$(kubectl -n "$NS" get pod -l app.kubernetes.io/instance="$HUB_RELEASE" \
    -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)"
  [[ -n "$HUB_IP" ]] && break
  sleep 2
done
if [[ -z "$HUB_IP" ]]; then
  echo "Failed to get hub Pod IP" >&2
  exit 1
fi
echo "Hub Pod IP: $HUB_IP"

echo "Deploying spokes ($SPOKE_RELEASE) peering to hub $HUB_IP ..."
helm upgrade --install "$SPOKE_RELEASE" "$CHART_DIR" -n "$NS" \
  --set hostNetwork=false \
  --set gobgp.enabled=true \
  --set gobgp.asn="$SPOKE_ASN" \
  --set gobgp.neighbors[0].address="$HUB_IP" \
  --set gobgp.neighbors[0].asn="$HUB_ASN" \
  --set gobgp.multihopTtl=8 \
  --set gobgp.rr.enabled=false \
  --set gobgp.rr.client=false \
  --set agent.localInterface=eth0 \
  --set agent.gobgpAddress=127.0.0.1:50051 \
  --set agent.advertiseSelf=true \
  --set image.tag="$IMAGE_TAG" \
  --set replicas="$SPOKE_REPLICAS" \
  --wait

# Dynamic-neighbor is enabled; spokes connect to hub by default. Uncomment for symmetric static neighbors.
# # Configure neighbors back on hub using spokes' PodIPs
# SPOKE_IPS=($(kubectl -n "$NS" get pod -l app.kubernetes.io/instance="$SPOKE_RELEASE" \
#   -o jsonpath='{range .items[*]}{.status.podIP}{" "}{end}'))
# 
# if [[ ${#SPOKE_IPS[@]} -gt 0 ]]; then
#   echo "Configuring hub neighbors -> spokes: ${SPOKE_IPS[*]}"
#   SET_NEIGH=()
#   idx=0
#   for ip in "${SPOKE_IPS[@]}"; do
#     SET_NEIGH+=(--set "gobgp.neighbors[$idx].address=$ip")
#     SET_NEIGH+=(--set "gobgp.neighbors[$idx].asn=$SPOKE_ASN")
#     idx=$((idx+1))
#   done
#   helm upgrade --install "$HUB_RELEASE" "$CHART_DIR" -n "$NS" \
#     --set hostNetwork=false \
#     --set gobgp.enabled=true \
#     --set gobgp.asn="$HUB_ASN" \
#     --set agent.localInterface=eth0 \
#     --set agent.gobgpAddress=127.0.0.1:50051 \
#     --set agent.advertiseSelf=true \
#     --set image.tag="$IMAGE_TAG" \
#     --set replicas="$HUB_REPLICAS" \
#     "${SET_NEIGH[@]}" \
#     "${NODE_SELECTOR_ARG[@]}" \
#     --wait
# fi

echo "Done. Current Pods:"
kubectl -n "$NS" get pods -l app.kubernetes.io/name=evpn-agent -o wide
