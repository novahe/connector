# 纯 TAP 调试网络 PoC 与实现方案

## 目标与边界

沙箱退出后，临时占用一个空闲的 connector TAP 端口，在 debug netns 内直接运行
`ping`、`curl`，使流量经过原 vSwitch 端口。PoC 不启动沙箱、不移动已有 TAP、
不使用旧 CNI 流程，也不修改运行中的 switch 配置。

`test/e2e/debug_tap_poc.py` 是针对当前 `e2br0919` 的一次性诊断工具，**不是**
可直接部署的通用命令。它固定使用该环境的 TAPFD socket、
`169.254.0.21/30`、网关 `169.254.0.22` 和管理 VIP `169.254.169.254`。

## 工作原理

```text
debug netns: ping/curl → Linux 内核 TCP/IP → dbg0 TAP → dbg0 fd
                                                    │ 以太网帧转发
switch netns:                              e2br0919-tN TAP fd
                                                    ↓
                                             e2br0919-tN 的 TC
                                                    ↓
                                      管理 veth → e2br0919_mgmt
```

调试 netns 的内核提供协议栈；转发器只处理 TAP 帧。原 TAP fd 按 connector
协议使用 `IFF_VNET_HDR` 和 12 字节 `virtio_net_hdr_v1`，调试 TAP fd 不带该头。
原 TAP 读出的帧若带 `NEEDS_CSUM`，转发器必须补完校验和后交给调试 TAP。
首轮 PoC 忽略该位时 ICMP 成功、TCP 超时；管理侧抓包显示 SYN/ACK
校验和未完成。补算后 TCP 完成握手并收到 HTTP 响应。PoC 对 GSO 帧明确报错，
正式实现需要支持分段或关闭相关 offload 后验证。

connector 的 `PREPARE` 将空闲端口置为 `Allocated`，正常沙箱分配不会使用它；
`Reserved` 状态不能作为调试端口，因为数据面不转发该状态。`OPEN` 取得
TAP fd，结束时 `RELEASE` 释放本次取得的端口。

## 复现与验证过程

在 `/root/sandbox/connector` 以 root 运行：

```bash
python3 -B test/e2e/debug_tap_poc.py
```

脚本自动完成：读取当前端口状态、申请空闲槽、创建临时 debug netns/TAP、
运行管理 VIP 的 ICMP 探测、在管理 netns 临时监听 `169.254.169.254:18080`
并进行 HTTP 探测、尝试 MMDS token 请求、打印端口计数、停止临时监听并释放端口。
管理侧抓包写入输出中提示的 `/tmp/tap-poc-<pid>-mgmt.pcap`。

2026-09-24 在 `e2br0919` 的结果：

| 检查 | 结果 |
| --- | --- |
| 申请前端口 | 1、2、3 已分配；PoC 取得空闲的 4 号 TAP 端口，floating IP `100.100.128.3` |
| ICMP | `ping 169.254.169.254`：2 发 2 收，0% 丢包 |
| 受控管理 HTTP | `curl http://169.254.169.254:18080/ok.txt` 返回 `tap-poc-ok` |
| TAP 转发 | debug→switch 18 帧，switch→debug 16 帧；其中 10 帧补算校验和 |
| vSwitch 计数 | 端口 4 的 `mgmt_tx_packets=14`、`mgmt_rx_packets=12`；transit 计数均为 0 |
| MMDS | `PUT /latest/api/token` 连接成功并送出请求，但 3 秒内未收到 HTTP 响应，`curl` 退出 28 |
| 清理 | `RELEASE` 成功；端口 4 恢复 `free`；无遗留 `tap-poc-*` netns 或 18080 监听 |

MMDS 的管理侧抓包显示：源 `100.100.128.3` 的 TCP 与转换目标
`127.0.0.1:49730` 完成握手，128 字节请求已被 ACK，但没有 HTTP 响应。
从 `e2br0919_mgmt` **直接**访问同一监听地址也超时。proxy 日志同时持续出现
`/run/sandbox-r0919/node-ctl.socket: no such file or directory` 的 route-sync
重连信息。因此 MMDS 的本次超时不能归因于 TAP 转发；服务侧为什么不响应需要
单独诊断，PoC 未修改该服务。

## 正式命令方案

增加 `connector-ctl vswitch debug run <switch> --ip <CIDR> --gateway <IP>
[--dns <IP>] [--port <空闲端口>] [--transit-*] -- <命令>`：

1. 验证参数和端口模式；用现有 attach 逻辑原子占用空闲 TAP 槽。指定端口已被
   沙箱占用时立即失败，不接管该端口。
2. 从 switch 元数据读取 switch netns，从 attach 结果读取端口 MAC 和 floating IP；
   创建临时 debug netns/TAP，配置与 build 相同的 IP、路由和 DNS。
3. 使用两端 TAP fd 转发完整以太网帧；显式处理 virtio 头、校验和、GSO、短写、
   退出信号与转发错误。转发器异常时终止被测命令并报告具体错误。
4. 执行交互 shell 或指定命令，结束后停止转发、删除临时资源并释放端口。
   强杀可能留下 `Allocated` 端口；正式版本应记录会话身份，提供只清理
   对应会话端口的恢复操作。

当前 `e2br0919` 的 switch 配置含管理路由 `0.0.0.0/0`，没有 transit 设备，
所以本次验证仅覆盖纯 TAP → 管理 netns 的全链路。要验证纯 TAP → Geneve
gateway，需要目标 switch 实际配置 transit 网卡/网关，并使外部测试目的地址
不落入管理 CIDR；再核对 `transit_tx/rx`、外层 Geneve 抓包、locator/VNI 与
双向应用响应。不能将本次管理链路成功当成 Geneve 已验证。

## socat 简化方案实测（2026-09-24）

在 `e2br0919` 使用系统安装的 socat 1.8.1.1 做了两轮临时测试。通过
TAPFD/1 `PREPARE` 取得空闲的 4 号端口，保持 `Allocated`；socat 在 switch
netns 按名打开原 `e2br0919-t4`，并用 `netns=` 在独立的临时 netns 创建
`dbg0`。关键启动参数是：

```bash
ip netns exec e2br0919_ns socat -d -d \
  TUN,tun-type=tap,tun-name=e2br0919-t4,no-pi \
  TUN,tun-type=tap,tun-name=dbg0,no-pi,netns=<临时-debug-netns>
```

debug TAP 配置 `169.254.0.21/30`、原端口 MAC 和经 `169.254.0.22` 的
默认路由。第一轮 `ping 169.254.169.254` 为 2/2，但受控管理 HTTP 的
`curl` 超时。管理侧抓包显示 SYN 正常到达，SYN/ACK 不断重发；结合此前
VNET_HDR PoC 的结果，回包未完成校验和是这次超时的原因。

第二轮仅在本次分配的 4 号 TAP 上临时关闭发送校验和、TSO 和 GSO：

```bash
ip netns exec e2br0919_ns ethtool -K e2br0919-t4 tx off tso off gso off
```

随后 ICMP 仍为 2/2，`curl http://169.254.169.254:18080/ok.txt` 收到
`HTTP 200` 和 `socat-poc-ok`；4 号端口管理方向计数为发送 8 包、接收 8 包。
停止 socat 后恢复该 TAP 的 `tx/tso/gso on`，删除临时 netns，通过
TAPFD/1 `RELEASE` 释放端口。事后核对端口 4 为 `free`，三个 offload
开关均恢复为 `on`。已有端口 1、2、3 未改动。

因此 socat 可以替代当前 PoC 的 Python 帧转发循环，但正式脚本必须在
`Allocated` 状态期间调整**本次端口**的 offload，并在异常退出时恢复。
本轮只验证管理方向的 ICMP 和短 HTTP；MMDS 服务响应、大报文、UDP 与
Geneve 流量仍需分别验证。当前 `e2br0919` 没有 transit 设备，不能在这里
声称 Geneve 已跑通。

## Shell 调试入口

已提供 [`scripts/vswitch-tap-debug.sh`](../scripts/vswitch-tap-debug.sh)。它不修改 connector
代码，只依赖节点已有的 `connector-ctl`、`ip`、`socat`、`ethtool`
以及基础 Shell 文本工具；不依赖 jq 或 Python。

查看英文参数说明：`scripts/vswitch-tap-debug.sh --help`（也支持 `-h`）。

默认用 `/opt/sandbox/bin/connector-ctl`，也可以通过
`CONNECTOR_CTL` 或 `CONNECTOR_CTL_PATH` 覆盖：

```bash
CONNECTOR_CTL=/opt/sandbox/bin/connector-ctl \
  scripts/vswitch-tap-debug.sh e2br0919
```

不带命令参数时，脚本会通过 `ip netns exec` 进入临时
`sandbox_debug_ns_<随机后缀>` 的交互 Shell；
在其中直接执行节点上的 `ping`、`curl` 或 `flatten-ctl`，输入 `exit` 后
自动停止 socat、恢复 TAP offload、删除 debug netns 并释放端口。临时
`/etc/netns/<ns>/resolv.conf` 默认写入 guest 使用的
`169.254.169.253`，进入 Shell 后 DNS 查询也使用这条路径：

```bash
scripts/vswitch-tap-debug.sh e2br0919
```

也可以执行单条命令后自动清理：

```bash
scripts/vswitch-tap-debug.sh e2br0919 ping -c 3 169.254.169.254
```

复现原沙箱的端口/FIP 时指定端口；若已占用，connector 的 attach 会失败，
脚本不会接管该端口：

```bash
scripts/vswitch-tap-debug.sh --port 4 e2br0919
# 或 DEBUG_PORT=4 scripts/vswitch-tap-debug.sh e2br0919
```

可用环境变量：

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `CONNECTOR_CTL` / `CONNECTOR_CTL_PATH` | `/opt/sandbox/bin/connector-ctl` | connector-ctl 路径 |
| `DEBUG_CIDR` | `169.254.0.21/30` | debug TAP 地址 |
| `DEBUG_GATEWAY` | `169.254.0.22` | TAP 默认网关 |
| `DEBUG_DNS` | `169.254.169.253` | debug ns 的 nameserver |
| `DEBUG_PORT` | 空（自动分配） | 固定原沙箱端口 |
| `TRANSIT_GATEWAY_IP` | 空 | Geneve transit gateway；为空时只走管理路径 |
| `TRANSIT_GENEVE_VNI` | 空 | Geneve VNI |
| `TRANSIT_MAC_ADDR` | 空 | 可选 transit 下一跳 MAC |
| `DEBUG_STATE_ROOT` | `/run/connector-debug-tap` | root 专用会话状态目录 |

例如 Geneve 调试：

```bash
TRANSIT_GATEWAY_IP=10.12.0.2 \
TRANSIT_GENEVE_VNI=100 \
scripts/vswitch-tap-debug.sh sw1
```

脚本会先确认 switch 为 Ready、switch netns 存在、依赖命令齐全，并检查
分配到的端口确实是 TAP。它在 `/run/connector-debug-tap` 保存本次端口、
netns、进程及完整的 `ethtool -k` 快照；退出时逐项比对并恢复 offload，
包括 `tx-tcp-mangleid-segmentation` 等依赖位。若恢复未通过校验，保留
`Allocated` 端口和会话记录，避免带着异常 offload 把端口交回池中。

SIGKILL/OOM 使 trap 无法执行时，查看和清理已失活的会话：

```bash
scripts/vswitch-tap-debug.sh --list
scripts/vswitch-tap-debug.sh --cleanup-stale
```

清理命令只处理记录中 owner PID 已结束的会话，并核对端口的 IP、FIP 和
ifindex。2026-09-24 在 `e2br0919` 对脚本执行 SIGKILL 后实测恢复成功：
端口 4 回到 `free`，临时 netns 删除，`tx/tso/gso` 恢复为 `on`，原本为
`off` 的 `tx-tcp-mangleid-segmentation` 仍为 `off`。

## 纯 TAP + Geneve 隔离验证（2026-09-24）

为验证完整路径，另外创建了两个临时 `--mode=tap` switch。每个 switch
配置一个 transit veth，端口 TAP 保留在各自 switch netns，再由 socat 接到
各自的 debug netns；没有使用 veth 端口或 CNI。两端配置
`geneve-port-base=52000`、VNI `100`，内层地址为 `10.13.0.1` 和
`10.13.0.2`。

验证结果：

| 检查 | 结果 |
| --- | --- |
| debug A → debug B ICMP | 2/2 成功，0% 丢包 |
| debug A → debug B HTTP | curl 返回 Python HTTP 服务页面 |
| switch A transit | RX 10 包/2049 字节，TX 10 包/858 字节 |
| switch B transit | RX 10 包/858 字节，TX 10 包/2049 字节 |
| 外层报文 | transit 设备抓到 UDP Geneve，端口为配置的 52000 |
| 清理 | 两个 switch、transit veth、socat 和 debug netns 均删除 |

完整路径如下：

```text
debug ns → debug TAP → socat → switch TAP
         → connector TC/BPF → transit veth 上的 Geneve UDP
         → 对端 connector 解封装 → 对端 switch TAP
         → socat → 对端 debug ns
```

正式 debug 命令需要在已有 transit 配置的 switch 上重复这套流程。验证时
同时保存两端 `vswitch stats` 和 transit 设备抓包；只看到 debug ns 的
`ping` 成功，不能单独证明流量走了 Geneve。

## 网上方案对比（2026-09-24）

现成工具中，已验证 `socat`：其手册支持 `TUN`、`tun-type=tap`、
`tun-name` 和每端独立的 `netns`，并给出了跨 namespace 双 TUN 转发示例。
因此可以考虑 shell 用 connector `attach` 占用端口，socat 按名打开该 TAP
及 debug netns 的 TAP，shell 配置 IP/路由并负责退出清理。这个方式不需要
接收 TAPFD/1 的 SCM_RIGHTS，也不需要自己写帧转发循环。

但 socat 的 `netns` 选项在手册中仍标注 experimental；本机安装的
1.8.1.1 已通过上述短流量测试。`no-pi` 模式下原 TAP 的发送 offload
必须关闭，其他版本及大报文的行为仍需验证。

另一项更贴近现有 TAPFD 契约的简化是：debug TAP 也启用 `IFF_VNET_HDR`，
两端统一头长度和字节序，转发器逐帧原样复制头与负载。Linux 的
`virtio_net_hdr_to_skb` 已有将校验和/GSO 元数据导入 skb 的实现。基于源码，
这一方案有望移除 PoC 自己补算校验和的代码；仍需实际验证 TCP、UDP、
大报文和 offload 配置，不能据此宣称所有 GSO 情况已支持。

`passt/pasta` 把二层流量映射成主机四层 socket。根据它的设计，这会改变本任务
要检查的原 TAP 端口转发路径，因此不作为当前方案的替代。

资料：

- [socat 手册（TUN、netns 与跨 netns 示例）](https://man7.org/linux/man-pages/man1/socat.1.html)
- [Linux TAP 驱动](https://github.com/torvalds/linux/blob/v6.6/drivers/net/tun.c)
- [Linux virtio_net_hdr_to_skb](https://github.com/torvalds/linux/blob/v6.6/include/linux/virtio_net.h)
- [passt/pasta 官方设计](https://passt.top/passt/about/)
