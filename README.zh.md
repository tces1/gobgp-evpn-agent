# GoBGP + Kernel VXLAN EVPN Agent (gobgp-evpn-agent)

用 Go 语言重写的 EVPN / VXLAN L2 自动发现实验：  
监听 GoBGP RIB（gRPC Watch），按 community 维护 kernel VXLAN FDB，支持多 VNI / 多节点，并提供可一键拉起的 Hub‑Spoke Helm Demo（Hub 作为 RR + dynamic neighbor，Spoke 静态对 Hub）。
定位为轻量级、类 zebra 的 EVPN/VXLAN 控制面替代（仅 L2 EVPN，不覆盖完整路由栈）。

## 特性
- 多 VNI：每个 VNI 映射一个 community，泛 MAC FDB 自动增删。
- 低占用：gRPC Watch 流式监听，无轮询；CGO 关闭，`-s -w` 构建，alpine 运行时镜像（业务容器）。
- kernel VXLAN：`nolearning`，仅用 FDB 作为转发表。
- 不使用 Linux Bridge，FDB 直接下发到 vxlan 设备。
- 可选自宣 membership：自动将本地 PodIP/32 + community 写入 RIB。
- Helm Demo：Deployment + gobgpd sidecar（同一 ASN，iBGP/多节点，或动态邻居 Hub‑Spoke）。

## 目录
- `cmd/evpn-agent`: 主程序入口
- `internal/`: 配置解析、BGP 监听、VXLAN/FDB 管理
- `charts/evpn-agent`: Helm Chart（Hub‑Spoke 脚本直接传参，示例 values 已移除以免混淆）
- `dockerfile/evpn-agent/Dockerfile`: 多阶段构建（alpine runtime）

## 构建
```sh
# 二进制
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/evpn-agent ./cmd/evpn-agent

# 容器
docker build -f dockerfile/evpn-agent/Dockerfile -t evpn-agent:dev .

# 或使用 Makefile（同时支持 kind load）
make docker
```

## 配置文件（/etc/evpn-agent/config.yaml）
```yaml
logLevel: info
advertiseSelf: true          # 是否自动把本地 /32 + community 写入 RIB
communityAsn: 65000          # vnis 未写 community 时，按 ASN:VNI 自动生成
gobgp:
  address: "127.0.0.1:50051" # gobgpd gRPC
  timeout: "5s"
node:
  localInterface: "eth0"     # 自动取该接口 IPv4；也可设置 localAddress
  localAddress: ""           # 留空自动探测
  vxlanPort: 4789
communityAsn: 65000
```
说明：
- **不会自动创建 vxlan**。agent 会扫描本机 vxlan，并按 `<communityAsn>:<vni>` 自动生成映射。
- 使用自动发现时必须设置 `communityAsn`。

## Helm 部署
前提：节点支持 VXLAN，Pod 需 `privileged` 或至少 `NET_ADMIN`。gobgp sidecar 直接执行 `gobgpd`，init 用 busybox 渲染配置模板。

### 部署（推荐：Hub‑Spoke 脚本，Hub 做 RR）
```sh
./scripts/deploy-hub-spokes.sh evpnlab
```
行为：
- Hub：dynamic-neighbors 前缀 10.244.0.0/16，同 ASN，peer-group 作为 RR（`rr.enabled=true, rr.client=true`）。
- Spoke：静态邻居指向 Hub，RR 关闭。
- 可用环境变量：`HUB_ASN`/`SPOKE_ASN`、`HUB_REPLICAS`/`SPOKE_REPLICAS`、`IMAGE_TAG`、`DYNAMIC_PREFIX`、`HUB_NODE`。

### 验证
- Hub 邻居：`kubectl -n evpnlab exec deploy/evpn-hub-evpn-agent -c gobgpd -- gobgp neighbor`
- Spoke 邻居：`kubectl -n evpnlab exec deploy/evpn-spokes-evpn-agent -c gobgpd -- gobgp neighbor`
- RIB：`kubectl -n evpnlab exec deploy/evpn-hub-evpn-agent -c gobgpd -- gobgp global rib -a ipv4 -j`
- FDB：`kubectl -n evpnlab exec deploy/evpn-hub-evpn-agent -c evpn-agent -- bridge fdb show`
- 数据面：为 vxlan 接口加测试 IP（如 10.10.10.x/24），互 ping。

> 不想用内置 gobgpd，可 `gobgp.enabled=false`，并把 `agent.gobgpAddress` 指向外部 gobgpd。

## 调试记录（关键踩坑与解决）
- 镜像问题：gobgpd sidecar 直接以 `command/args` 运行；init 用 busybox 渲染模板；业务镜像加入 iproute2 便于查看/操作 FDB。
- FDB 写入失败（EOPNOTSUPP）：改用 `netlink.NeighAppend`，保持 VXLAN `Learning=false`，并在 runtime 安装 iproute2；之后手工/自动都能写入 flood FDB。
- iBGP 不互通：Hub 未正确反射；将 Hub 设置为 RR（peer-group route-reflector-client=true），Spoke 关闭 RR；删除旧 ConfigMap 重新部署后，spoke RIB 获得所有 /32。
- 最终验证：在两个 Spoke 上 **手工创建** `vxlan10010` 并配置 10.10.10.x/24，跨节点 ping 成功；FDB 包含所有 VTEP 的 00:00:00:00:00:00 泛洪条目。
- 撤销验证：删除某个 Spoke 的 `vxlan10010` 后，agent 触发 BGP withdraw，其他节点 RIB 中该 /32 被撤销；数据面 ping 失败（符合预期）。
- 多 VNI 隔离：Pod1/Pod2 共享 `vxlan10010`，Pod1/Pod3 共享 `vxlan10011`。同 VNI ping 成功，跨 VNI ping 失败；FDB 按 VNI 隔离。

## 原理概览
evpn-agent 做两件事：  
1) **监听 BGP RIB**：通过 gobgp gRPC `WatchEvent(BEST)` 订阅 IPv4-unicast 路由；根据 community -> VNI 映射，把路由前缀（/32）转化为远端 VTEP 列表。  
2) **驱动内核 VXLAN**：每个 VNI 对应一个 `vxlan<ID>` 接口；agent 将远端 VTEP 写入 FDB（MAC 全 0，DST=对端 PodIP），实现无泛洪的 L2 可达。默认不自动创建 vxlan——需要你手动 `ip link add vxlan<ID> ...`，agent 发现后才自宣；手动删除则撤销自宣并停止同步（`agent.autoRecreateVxlan` 默认为 `false`，设为 `true` 才会自动重建）。

数据流（简化 ASCII）：
```
gobgpd (RR / peers)
   │ WatchEvent (best paths)
   ▼
evpn-agent
   ├─ community 65000:VNI -> VNIConfig
   ├─ desired[VNI] = {remote /32 prefixes}
   ▼
vxlan.Manager
   ├─ ensure vxlan<ID> exists（不自动创建，除非 autoRecreateVxlan=true）
   └─ NeighAppend FDB: 00:00:00:00:00:00 dst <remote VTEP>
```
自宣：`advertiseSelf=true` 时，agent 将本地 /32 + community 写入 gobgpd，使其他节点能生成 FDB。

调试机制与常用命令
- **BGP**：`kubectl exec deploy/evpn-hub-evpn-agent -c gobgpd -- gobgp neighbor` / `gobgp global rib`
- **配置确认**：`kubectl exec <pod> -c gobgpd -- gobgp global`（查看 AFI/SAFI 能力）、`cat /etc/gobgpd/gobgpd.toml`
- **FDB/链路**：`kubectl exec <spoke> -c evpn-agent -- bridge fdb show dev vxlan10010`，`ip -d link show vxlan10010`
  - 手工创建/删除 vxlan 验证加入/撤销：  
    `ip link add vxlan10010 type vxlan id 10010 local <POD_IP> dev eth0 dstport 4789 nolearning`  
    `ip link set vxlan10010 up`  
    删除后观察 BGP 路由撤销、FDB 清空。
- **数据面**：在不同节点给 vxlan10010 配 IP，互 ping 验证。
- **模板渲染**：若 ConfigMap 更新不生效，删除对应 CM 后重跑 `deploy-hub-spokes.sh` 以强制刷新。
- **日志**：`kubectl logs <pod> -c evpn-agent` 关注 `sync fdb failed`，`kubectl logs <pod> -c gobgpd` 关注邻居状态/错误。

## 快速复现步骤
```sh
# 1) 构建并加载 evpn-agent 镜像（sidecar gobgp 用 jauderho/gobgp:v3.28.0）
make docker

# 2) 部署 hub + spokes（Hub 做 RR + dynamic neighbor）
./scripts/deploy-hub-spokes.sh evpnlab

# 3) 验证 BGP
kubectl -n evpnlab exec deploy/evpn-hub-evpn-agent -c gobgpd -- gobgp neighbor
kubectl -n evpnlab exec deploy/evpn-spokes-evpn-agent -c gobgpd -- gobgp global rib

# 4) 数据面验证（先手工创建 vxlan，再加 IP）
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip link add vxlan10010 type vxlan id 10010 local <POD_IP1> dev eth0 dstport 4789 nolearning
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip link set vxlan10010 up
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip link add vxlan10010 type vxlan id 10010 local <POD_IP2> dev eth0 dstport 4789 nolearning
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip link set vxlan10010 up
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ip addr add 10.10.10.2/24 dev vxlan10010
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ip addr add 10.10.10.3/24 dev vxlan10010
kubectl -n evpnlab exec <spoke2-pod> -c evpn-agent -- ping -c 3 10.10.10.2

# 5) 多 VNI 隔离测试
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

# 验证隔离
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ping -I vxlan10010 -c 3 10.10.10.3
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ping -I vxlan10011 -c 3 10.11.11.3
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ping -I vxlan10010 -c 3 10.11.11.3 || true
kubectl -n evpnlab exec <spoke1-pod> -c evpn-agent -- ping -I vxlan10011 -c 3 10.10.10.3 || true
```

## 运行时说明
- 守护协程：通过 GoBGP `WatchEvent` 订阅 BEST 路径，匹配 community -> VNI。
- FDB 同步：仅对 `00:00:00:00:00:00` 泛 MAC 维护 `bridge fdb`.
- 链路清理：默认退出时删除创建的 VXLAN 接口；如需保留，`node.skipLinkCleanup=true`。
