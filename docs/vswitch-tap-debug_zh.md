# vswitch-tap-debug.sh 操作与测试手册

免 VM 的沙箱网络调试工具：在宿主机上临时占用一个 connector TAP 端口，把一个
disposable netns 桥接到该端口上，netns 内的 Linux 内核栈作为"假沙箱"直接跑
`ping` / `curl` / `flatten-ctl`，流量经过与真沙箱**逐字节相同**的 BPF 数据面
（FIP SNAT、mgmt-extract、per-port 计数）。

```text
debug netns(内核栈) ── dbg0 ── socat(双 TAP 桥) ── <switch>-tN (端口池内, 原位打开)
                                                      │ BPF TC (FIP/mgmt-extract)
                                                   mgmt netns ── (NAT/DNAT) ── 外网
```

背景与原理推导见 [debug-tap-poc_zh.md](debug-tap-poc_zh.md)（双 TAP PoC、socat
简化、Geneve 隔离验证）。
Python 标准库替代方案及流程图见
[vswitch-tap-debug-python-design_zh.md](vswitch-tap-debug-python-design_zh.md)。
Go 集成版设计及流程图见
[vswitch-debug-design_zh.md](vswitch-debug-design_zh.md)。

## Go 集成版 `vswitch debug`

```bash
# 仅有一个 switch 时自动选择；多个 switch 时从终端选择
connector-ctl vswitch debug
connector-ctl vswitch debug --port 4 e2br0919
connector-ctl vswitch debug e2br0919 -- curl -sS -m5 https://<endpoint>/
connector-ctl vswitch debug --transit-gateway-ip 10.12.0.2 \
    --transit-geneve-vni 100 sw1 -- ping -c 3 10.1.0.2

# 调整 attach 的超时（秒），并查看或恢复 Go 版会话
connector-ctl vswitch debug --ctl-timeout 30 e2br0919
connector-ctl vswitch debug --list
connector-ctl vswitch debug --cleanup-stale
```

Go 版支持同名的 `DEBUG_PORT`、`DEBUG_CIDR`、`DEBUG_GATEWAY`、`DEBUG_DNS`、
`DEBUG_TAIL_TRIES`、`DEBUG_CTL_TIMEOUT`、`DEBUG_KEEP_PROXY`、`DEBUG_STATE_ROOT`
及 `TRANSIT_*` 环境变量；显式命令行参数优先。默认会话目录为
`/run/connector-debug`，每个会话使用 `state.json`。Shell 版仍使用
`/run/connector-debug-tap` 和 key=value 的 `session` 文件。两版会话互不干扰，
也互不读取或恢复；混用时需分别执行两版的 `--cleanup-stale`。Go 版运行时
无需 `socat`、`nsenter`、`ip` 或 `ethtool` 命令；默认交互会话仍使用宿主机的 Bash。

Go 版私有 switch 的管理 VIP、端口恢复与双 switch Geneve E2E：

```bash
go build -o /tmp/connector-ctl-debug-e2e ./cmd/connector-ctl
CONNECTOR_CTL=/tmp/connector-ctl-debug-e2e python3 test/e2e/vswitch_debug_test.py
```

## 快速上手

```bash
# 交互 shell（type exit 退出并自动还原：停 socat、恢复 offload、删 netns、释放端口）
CONNECTOR_CTL=/path/to/connector-ctl ./scripts/vswitch-tap-debug.sh e2br0919

# 单命令
CONNECTOR_CTL=... ./scripts/vswitch-tap-debug.sh e2br0919 curl -sS -m5 https://<endpoint>/

# 钉死端口（复现某个故障沙箱当时的 FIP / 端口语义）
CONNECTOR_CTL=... ./scripts/vswitch-tap-debug.sh --port 4 e2br0919

# Geneve/transit 环境（参数与故障沙箱当时的 network spec 一致才有复现价值）
TRANSIT_GATEWAY_IP=10.12.0.2 TRANSIT_GENEVE_VNI=100 CONNECTOR_CTL=... \
    ./scripts/vswitch-tap-debug.sh sw1
```

## 命令行与环境变量

| 参数 | 说明 |
| --- | --- |
| `--port N` | 精确占用端口 N，被占即失败 |
| （省略） | auto 模式：选**最尾部**的空闲 TAP 槽（见下节） |
| `--list` | 列出会话、FIP、创建时间及 active/stale 状态；不要求 connector-ctl 可用 |
| `--cleanup-stale` | 清理属主进程已退出的会话 |
| `-h` | 完整帮助 |

| 环境变量 | 默认 | 说明 |
| --- | --- | --- |
| `CONNECTOR_CTL` / `CONNECTOR_CTL_PATH` | `/opt/sandbox/bin/connector-ctl`（不在则取 PATH） | connector-ctl 路径 |
| `DEBUG_CIDR` / `DEBUG_GATEWAY` | `169.254.0.21/30` / `169.254.0.22` | 内层 /30（与 e2b profile 一致） |
| `DEBUG_DNS` | `169.254.169.253` | netns 内 resolv.conf，走 guest 同款 DNS 路径 |
| `DEBUG_TAIL_TRIES` | `8` | auto 模式在后半池最多考虑的尾部槽数，耗尽即失败 |
| `DEBUG_CTL_TIMEOUT` | `30` | 每次 connector-ctl 调用的超时秒数；超时后 5 秒强制结束 |
| `DEBUG_KEEP_PROXY` | 空 | 默认**剔除**全部 host 代理变量（guest 无代理；不剔会把"网络不通"伪造成代理故障） |
| `DEBUG_STATE_ROOT` | `/run/connector-debug-tap` | 会话状态目录 |
| `TRANSIT_GATEWAY_IP` / `TRANSIT_GENEVE_VNI` / `TRANSIT_MAC_ADDR` | 空 | attach 的 transit 参数 |

## 端口分配语义

- 沙箱分配器（`FindFreeSlot`）**从队头线性扫描**。auto 模式若也拿最前空闲口，
  会精确插队到下一个沙箱的位置：后续沙箱 FIP 整体后移，池将满时直接顶掉一次
  launch。
- 因此 auto 模式只看后半池中端口号最大的 `DEBUG_TAIL_TRIES` 个槽，过滤 `state=free &&
  mode=tap` 后从高到低尝试（CAS 天然防同槽双占），
  全部被抢则报错退出，**绝不回落到前排**。
- 显式 `--port` 用于复现：端口决定 FIP（`FIP = floating_ip_base + port - 1`）。
- 只有 connector 明确返回“端口已分配”时才尝试下一个候选。attach 子进程被杀或
  返回结果不完整时保留待分配口记录，避免丢失已经成功分配的槽位。

## 会话生命周期与清理

| 场景 | 行为 |
| --- | --- |
| 正常退出 / INT / TERM / HUP | 精确查找并终止本次 socat 与 debug netns 中的后台进程，确认 TAP fd 已关闭 → 逐位恢复 offload（完整 `ethtool -k` 快照 + diff 校验）→ 删 netns 与 resolv.conf → 复核所有权和 TAP fd 后 detach；任何一步无法确认都保留端口和记录 |
| SIGKILL / OOM | 残留四件套（allocated 端口 + offload off + netns + socat）；会话状态在 `/run/connector-debug-tap/session.*`（owner=pid+starttime，防 PID 复用），用 `--cleanup-stale` 恢复 |
| 端口已易主（被人工释放后重新分配） | 若槽位信息不同，或检测到另一个进程仍持有 TAP fd，**不 detach、不碰新占有者的 offload**；仍清理可确认属于本会话的资源，会话目录保留供取证。同 IP 复用且尚未打开 TAP 的限制见下文 |
| 并发 `--cleanup-stale` | 对同一会话文件加 `flock`，最多等待 60 秒；后到者复查文件是否仍在，避免重复恢复或 detach |
| attach 中途被 SIGKILL | 分配前记录待分配口；子进程持有 attach 锁并写入退出码和回执。若子进程也被杀或结果无法确认，保留端口与记录供人工核对 |
| connector-ctl 超时 | attach 结果不确定时保留待分配口记录；清理阶段查询失败时先回收可确认属于会话的进程，但保留端口和记录供重试 |
| debug netns 同名重建 | 先按本会话的 TAP/netns/socat 参数回收旧 socat；不删除新 netns、不释放端口，保留记录供检查 |

## 回归套件 `vswitch-tap-debug-test.sh`

**自包含**：默认自带私有 switch（`vswitch start dttest`：8 口 tap 模式 + 专用
mgmt netns + VIP + FIP /20），跑完 `vswitch stop --force` 拆净（含 bpffs pin），
不触碰生产端口池，不限定某台节点。

```bash
CONNECTOR_CTL=/path/to/connector-ctl ./scripts/vswitch-tap-debug-test.sh           # 自包含
CONNECTOR_CTL=/path/to/connector-ctl python3 test/e2e/vswitch_tap_debug_safety_test.py # 私有 switch 故障注入
CONNECTOR_CTL=... ./scripts/vswitch-tap-debug-test.sh --use e2br0919               # 对真实环境跑（含外部链路探测）
KEEP_TEST_SWITCH=1 ./scripts/vswitch-tap-debug-test.sh                            # 留下私有 switch 排障
SKIP_PROBES=1 ./scripts/vswitch-tap-debug-test.sh --use e2br0919                   # 跳过外部探测
```

用例矩阵（当前 20 PASS / 1 SKIP）：

| 分组 | 用例 |
| --- | --- |
| 私有 switch 生命周期 | 启动并 Ready；拆除无 bpffs/netns 残留 |
| 入口与参数 | `-h`=0；无参数 usage=2；坏 switch / 坏 CONNECTOR_CTL 干净失败 |
| auto 模式 | 尾部选口（> total/2）；ICMP→VIP；代理变量剔除；`DEBUG_KEEP_PROXY`；MTU 跟随池 TAP；会话后端口归还 |
| 显式 `--port` | 会话+探测；端口归还 |
| 外部链路（仅 `--use`） | DNS 走 guest 路径（.253→DNAT）；外部 HTTPS 全链路 |
| SIGKILL 恢复 | `--list` 识别 stale；`--cleanup-stale` 完整恢复 |
| 端口易主 | 不误 detach 新占有者；自有资源照清；目录保留 |
| 多 agent 并发 | 双 agent 各得不同尾部口；并发 cleanup-stale 终态正确 |
| offload 无痕 | 会话前后 offload **实际值**逐位一致 |

故障注入另有 14 项：attach 子进程中断、父进程在 attach 中途退出、socat PID
未落盘且进程停住、同 IP 复用且新进程已打开 TAP、快照缺失与恢复失败、查询失败、
后台进程忽略 TERM、仅前排空闲、connector-ctl 查询或 attach 挂起、debug netns
同名重建、不依赖 connector-ctl 的 `--list`、旧版 `--ready` 错误透传等。只在私有
switch 上运行，Python 仅作为测试依赖。

新增用例约定：向对应分组追加一个块，断言用 `result pass|fail|skip`；kill 场景必须
**直接**启动 `"$DT"`（经函数包装后台化时 `$!` 是子 shell，kill -9 杀不到脚本本体）。

## 平台行为备忘（踩过的坑）

- **offload 翻转只在 mgmt 路径必须**：BPF SNAT 改写会留下 `CHECKSUM_PARTIAL`，
  无 vnet_hdr 元数据时回包校验和缺失、TCP 挂；Geneve/transit 路径整包重封装，
  实测无需翻转。恢复必须 `ethtool -k` **全量快照逐位还原**（`tso on` 会连带翻
  依赖位 `tx-tcp-mangleid-segmentation`）。
- **池内口 offload 的"出厂态"是 off**；被真沙箱（VMM 以 `IFF_VNET_HDR` 开 fd）
  用过的口会变成 on 并保持。对照组要用同组口，勿拿 t1 对比高编号口。
- **ethtool netlink 存在数十秒级瞬时失效窗口**（rtnetlink 正常、静置自愈、`-K`
  客户端报错时内核侧可能已生效）：所有 ethtool 调用必须带重试。
- **tap fd 独占**：socat 持着 fd 时其他 `open-port` 可能得到 EBUSY——这是残留 socat 必须
  杀干净的原因之一。
- **不要直接对池内 tap 做参数实验**；清理临时 switch 必须 `vswitch stop`（手删
  netns/veth 会留 bpffs pin 僵尸）。

## 已知限制与后续

1. 端口所有权复核（inner_ip / ifindex / FIP）在**同 profile 复用**下无法区分新
   旧占有者（三者都是端口的确定函数）。若有人手工释放 stale 口并用同一 IP
   重新分配，但新占有者尚未打开 TAP，清理脚本仍可能误判；**不要**手工释放 stale
   口。对已经打开 TAP 的新占有者，清理会检测 fd 并保留端口。若要消除所有代次
   竞态，connector 仍需提供分配代次和原子条件 detach。
2. `vswitch status --ready` 若旧版二进制不支持，会显示原始错误（如 unknown flag）。
3. `ip` 和 `ethtool` 各自有有限次数的重试，但单次调用没有独立超时；内核调用若
   长时间卡住，仍可能拖住清理。`connector-ctl` 调用已有独立超时。
4. 出厂-off 口上会话会留 `tx-udp-segmentation` wanted=on 的 cosmetic 痕迹（实际
   值不受影响，下次真沙箱 attach 自愈）。
