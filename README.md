# GoBGP + Kernel VXLAN EVPN Agent (gobgp-evpn-agent)

A Go implementation of an EVPN/VXLAN L2 auto-discovery lab:
- Watches GoBGP RIB (gRPC Watch)
- Maintains kernel VXLAN FDB by community
- Supports multi-VNI and multi-node
- One-command Hub/Spoke Helm demo (Hub as RR + dynamic neighbors, Spokes peer to Hub)
Positioned as a lightweight, zebra-like EVPN/VXLAN control-plane replacement (L2 EVPN only; not a full routing stack).

## Features
- Multi-VNI: each VNI maps to a community; flood FDB entries are added/removed automatically.
- Low overhead: gRPC Watch stream, no polling; CGO disabled, `-s -w` build, alpine runtime image.
- Kernel VXLAN: `nolearning`, FDB-only forwarding.
- Linux Bridge is intentionally not used; FDB is programmed directly on the vxlan device.
- Optional membership advertisement: announce local PodIP/32 with community into RIB.
- Helm demo: Deployment + gobgpd sidecar (iBGP, RR hub, dynamic neighbors).

## Layout
- `cmd/evpn-agent`: entrypoint
- `internal/`: config parsing, BGP watcher, VXLAN/FDB management
- `charts/evpn-agent`: Helm chart (Hub/Spoke script passes values; sample values removed to avoid confusion)
- `dockerfile/evpn-agent/Dockerfile`: multi-stage build (alpine runtime)

## Build
```sh
# binary
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/evpn-agent ./cmd/evpn-agent

# container
docker build -f dockerfile/evpn-agent/Dockerfile -t evpn-agent:dev .

# or use Makefile (also supports kind load)
make docker
```

## Config (`/etc/evpn-agent/config.yaml`)
```yaml
logLevel: info
advertiseSelf: true          # announce local /32 + community into RIB
communityAsn: 65000          # auto community = ASN:VNI when vnis entry omits community
gobgp:
  address: "127.0.0.1:50051" # gobgpd gRPC
  timeout: "5s"
node:
  localInterface: "eth0"     # detect IPv4 from this interface
  localAddress: ""           # empty = auto-detect
  vxlanPort: 4789
communityAsn: 65000
```
Notes:
- **VXLAN is not created automatically.** The agent discovers local VXLAN links and derives community as `<communityAsn>:<vni>`.
- `communityAsn` must be set when using auto-discovery.

## Helm Deployment
Prereqs: nodes must support VXLAN; pods require `privileged` or at least `NET_ADMIN`. The gobgpd sidecar runs `gobgpd` directly; init uses busybox to render the config template.

### Deploy (recommended: Hub/Spoke, Hub is RR)
```sh
./scripts/deploy-hub-spokes.sh evpnlab
```
Behavior:
- Hub: dynamic-neighbor prefix `10.244.0.0/16`, same ASN, peer-group as RR (`rr.enabled=true, rr.client=true`).
- Spoke: static neighbor to Hub, RR disabled.
- Env vars: `HUB_ASN`/`SPOKE_ASN`, `HUB_REPLICAS`/`SPOKE_REPLICAS`, `IMAGE_TAG`, `DYNAMIC_PREFIX`, `HUB_NODE`.

### Verify
- Hub neighbors: `kubectl -n evpnlab exec deploy/evpn-hub-evpn-agent -c gobgpd -- gobgp neighbor`
- Spoke neighbors: `kubectl -n evpnlab exec deploy/evpn-spokes-evpn-agent -c gobgpd -- gobgp neighbor`
- RIB: `kubectl -n evpnlab exec deploy/evpn-hub-evpn-agent -c gobgpd -- gobgp global rib -a ipv4 -j`
- FDB: `kubectl -n evpnlab exec deploy/evpn-hub-evpn-agent -c evpn-agent -- bridge fdb show`
- Data plane: add IPs to vxlan, then ping.

> If you don’t want the built-in gobgpd, set `gobgp.enabled=false` and point `agent.gobgpAddress` to an external gobgpd.

## Debug Notes (Key Findings)
- Gobgpd sidecar runs via `command/args`; init uses busybox to render config; runtime image includes iproute2 for FDB ops.
- FDB append uses `netlink.NeighAppend` with VXLAN `Learning=false` to allow multiple flood entries.
- iBGP RR: Hub must be RR, Spokes are clients; recreate ConfigMaps if Helm updates don’t apply.
- Final verify: create `vxlan10010` on two Spokes, set 10.10.10.x/24, cross-node ping succeeds; FDB includes flood entries.
- Withdrawal verify: deleting `vxlan10010` on a Spoke triggers BGP withdraw and remote RIB removal; ping fails as expected.
- Multi-VNI isolation: create `vxlan10010` between Pod1/Pod2 and `vxlan10011` between Pod1/Pod3. In-VNI pings succeed; cross-VNI pings fail. FDB entries stay separate per VNI.

## How It Works
The agent does two things:
1) **Watch BGP RIB**: `WatchEvent(BEST)` over gRPC. Community → VNI mapping turns /32 prefixes into remote VTEP list.
2) **Drive kernel VXLAN**: for each VNI, write remote VTEPs into FDB (MAC 00:00:00:00:00:00, dst=PodIP). VXLAN is not created automatically.

Data flow (simplified):
```
 gobgpd (RR / peers)
    │ WatchEvent (best paths)
    ▼
 evpn-agent
    ├─ community 65000:VNI -> VNIConfig
    ├─ desired[VNI] = {remote /32 prefixes}
    ▼
 vxlan.Manager
    └─ NeighAppend FDB: 00:00:00:00:00:00 dst <remote VTEP>
```

`advertiseSelf=true` writes local /32 + community into gobgpd so other nodes can build FDB entries.

## Debug & Common Commands
- **BGP**: `kubectl exec deploy/evpn-hub-evpn-agent -c gobgpd -- gobgp neighbor` / `gobgp global rib`
- **Config**: `kubectl exec <pod> -c gobgpd -- cat /etc/gobgpd/gobgpd.toml`
- **FDB/Link**: `kubectl exec <spoke> -c evpn-agent -- bridge fdb show dev vxlan10010` and `ip -d link show vxlan10010`
  - Create/withdraw test:
    `ip link add vxlan10010 type vxlan id 10010 local <POD_IP> dev eth0 dstport 4789 nolearning`
    `ip link set vxlan10010 up`
- **Data plane**: add IPs to vxlan on two nodes, then ping.
- **Template refresh**: if ConfigMap changes don’t apply, delete the CM and rerun `deploy-hub-spokes.sh`.
- **Logs**: `kubectl logs <pod> -c evpn-agent` for `sync fdb failed`; `kubectl logs <pod> -c gobgpd` for neighbor status.

## Quick Repro
```sh
# 1) Build and load agent image (sidecar gobgp uses jauderho/gobgp:v3.28.0)
make docker

# 2) Deploy hub + spokes (Hub as RR + dynamic neighbor)
./scripts/deploy-hub-spokes.sh evpnlab

# 3) Verify BGP
kubectl -n evpnlab exec deploy/evpn-hub-evpn-agent -c gobgpd -- gobgp neighbor
kubectl -n evpnlab exec deploy/evpn-spokes-evpn-agent -c gobgpd -- gobgp global rib

# 4) Data-plane test (manual vxlan creation + IPs)
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip link add vxlan10010 type vxlan id 10010 local <POD_IP1> dev eth0 dstport 4789 nolearning
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip link set vxlan10010 up
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip link add vxlan10010 type vxlan id 10010 local <POD_IP2> dev eth0 dstport 4789 nolearning
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip link set vxlan10010 up
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip addr add 10.10.10.2/24 dev vxlan10010
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip addr add 10.10.10.3/24 dev vxlan10010
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ping -c 3 10.10.10.2

# 5) Multi-VNI isolation test
# VNI 10010: spoke1 <-> spoke2
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip link add vxlan10010 type vxlan id 10010 dev eth0 dstport 4789 nolearning
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip link set vxlan10010 up
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip addr add 10.10.10.2/24 dev vxlan10010
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip link add vxlan10010 type vxlan id 10010 dev eth0 dstport 4789 nolearning
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip link set vxlan10010 up
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip addr add 10.10.10.3/24 dev vxlan10010

# VNI 10011: spoke1 <-> spoke3
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip link add vxlan10011 type vxlan id 10011 dev eth0 dstport 4789 nolearning
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip link set vxlan10011 up
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip addr add 10.11.11.2/24 dev vxlan10011
kubectl -n evpnlab exec <spoke3-pod> -c evpn-agent -- ip link add vxlan10011 type vxlan id 10011 dev eth0 dstport 4789 nolearning
kubectl -n evpnlab exec <spoke3-pod> -c evpn-agent -- ip link set vxlan10011 up
kubectl -n evpnlab exec <spoke3-pod> -c evpn-agent -- ip addr add 10.11.11.3/24 dev vxlan10011

# Verify isolation
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ping -I vxlan10010 -c 3 10.10.10.3
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ping -I vxlan10011 -c 3 10.11.11.3
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ping -I vxlan10010 -c 3 10.11.11.3 || true
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ping -I vxlan10011 -c 3 10.10.10.3 || true
```

## Runtime Notes
- Goroutine: GoBGP `WatchEvent` streams BEST paths and maps community → VNI.
- FDB sync: only flood MAC `00:00:00:00:00:00` entries are maintained.
- Link cleanup: by default the agent deletes VXLAN interfaces on exit; set `node.skipLinkCleanup=true` to keep them.


## 中文说明
See `README.zh.md`.
