# 远程来电失败：信令、TURN 和音频阶段排查

2026-10-09，针对 `home-xiaomi` / `dji-75d81d083dca` / `https://oops.work`。

## 本次只读检查结果

- Hub profile 为 ready；路由为 remote，Opus 1.6.1 已启用；Hub 常驻信令 online。
- Go 后端和 Swift 宿主均使用同一个 native bootstrap 文件。
- Relay `https://oops.work/healthz` 返回 200，identity/signaling/turn 为 true。
- Mac 启用了 Surge 系统代理，且 DNS 将 `oops.work` 解析为 `198.18.206.233`。Surge 的增强模式会使用这种虚拟 IP，见 [官方说明](https://manual.nssurge.com/features/enhanced-mode.html)。
- 公共 DNS 查询得到 IPv4 `47.108.184.34`。在当时的 Mac 网络环境中，域名和该真实 IP 的 TCP 5349 均能连接，但保留 `oops.work` 域名验证的 TLS 握手在 8 秒预算内没有完成。

这证明当前 TURN/TLS 路径还未验证成功；真实 IP 探测仍从同一 Mac 网络发出，不能据此断言一定是 coturn 服务端故障或一定是 Surge 故障。HTTPS/WSS online 不代表 TURN 分配、ICE/DTLS 或双向媒体可用。

## 如何理解当前输出

- `available:false` / `online_only_not_production`：在线联调版保留的整体状态，不能单独用它判断此次失败。
- `remote check` 报 `call signaling connection unavailable`：该命令会建立新的 Hub WSS；常驻 Hub 已在线时，第二条连接被 Relay 拒绝。需要 check 时先停常驻 Hub，检查后再启动；在线期间查询本机 status 即可。
- `Failed to close relay connection ... use of closed network connection`：关闭已关闭 socket 的清理日志。它可能跟随失败出现，也可能发生在正常收尾；不能仅凭这行定位原因。
- `module voice preflight ... context canceled`：原通话上下文结束后，准备工作取消。它可能是远端失败/主叫挂断后的结果；需要结合会话结束原因判断。
- `remote status` 的 access_expires_at：Keychain 中保存记录的到期时间，不是当前 socket 的握手健康探针；凭据由常驻 worker 在到期前轮换。不要为此重复注册或覆盖 ready profile。

## 先做网络隔离

第一轮临时关闭 Mac Surge 的增强模式与系统代理；iPhone 若有 VPN/Surge，也暂时关闭。重新连接 iPhone App，保持前台；等 Hub online 后重新拨打新来电。

也可以保留 Surge，按现有配置合并以下条目，而非覆盖整个配置：

```ini
[General]
always-real-ip = oops.work

[Rule]
DOMAIN,oops.work,DIRECT
```

原有 `always-real-ip` 项要保留，将 oops.work 追加进去；DIRECT 规则应置于会提前匹配的代理规则/FINAL 之前。若使用其他 TURN 域名，也必须加入相应 real-IP 和直连规则。仅 DIRECT 规则不等于客户端一定能取得真实 DNS 地址；`always-real-ip` 的语义见 [Surge 官方 DNS 说明](https://manual.nssurge.com/dns/advanced.html)。

重载 Surge 配置并处理其/系统的 DNS 缓存后，在 Mac 检查：

```sh
dscacheutil -q host -a name oops.work
openssl s_client -connect oops.work:5349 -servername oops.work \
  -verify_hostname oops.work -verify_return_error </dev/null
```

期望域名不再返回 198.18/198.19 虚拟地址，TLS 检查完成且证书验证成功。关闭代理仅用于对比，测试后按自己的网络需求恢复。

## 在 CentOS 上检查 coturn

以下命令只读，不更换密钥、不重装服务：

```sh
sudo systemctl status djonehub-turn --no-pager
sudo ss -lntup | grep -E ':(3478|5349)\b'
sudo journalctl -u djonehub-turn -n 100 --no-pager
sudo grep -E '^(listening-port|tls-listening-port|listening-ip|relay-ip|external-ip|realm|cert|pkey|no-tls|no-dtls)' \
  /etc/djonehub/turnserver.conf
sudo jq '{urls,relay_only}' /etc/djonehub/turn-issuer.json
sudo -u djonehub openssl x509 -in /etc/djonehub/tls/fullchain.pem \
  -noout -subject -issuer -dates
sudo -u djonehub openssl pkey -in /etc/djonehub/tls/privkey.pem -noout -check
```

重点检查 TLS listener 是否启用、运行用户是否能读取证书/私钥，以及日志中 certificate、private key、TLS、bind 等错误。不要输出 issuer 的 secret 或私钥内容。TCP connect 成功不能代替 TLS handshake 成功。

在服务器本机按实际监听 IP 测试 TLS，可排除 Mac 的代理路径：

```sh
DJ_TURN_BIND_IP=$(sudo awk -F= '/^listening-ip=/ {print $2; exit}' /etc/djonehub/turnserver.conf)
case "$DJ_TURN_BIND_IP" in
  ''|0.0.0.0) DJ_TURN_BIND_IP=127.0.0.1 ;;
esac
timeout 12 openssl s_client -connect "$DJ_TURN_BIND_IP:5349" \
  -servername oops.work -verify_hostname oops.work -verify_return_error </dev/null
```

本机也失败时优先检查 coturn TLS 配置与证书读取；本机通过、Mac 失败时继续检查云安全组/网络路径/Surge。TURN/TLS 是独立协议，不能通过普通 Nginx HTTP location 反代。服务器 UDP relay 端口范围仍须开放，即使客户端使用 TURN/TCP 或 TLS。

## 分离 UDP / TCP / TLS

按 [安装指南的分项验收](REMOTE_IPHONE_INSTALL_AND_TEST.md)做单 URL 测试：保持 `relay_only:true`，每轮只保留一条 `turn:...3478?transport=udp`、`turn:...3478?transport=tcp` 或 `turns:...5349?transport=tcp`，不改 secret。

修改 issuer 后在无通话时重启 Relay；这会断开信令，iPhone 需要手动重新连接，Hub 会重连。必须等待 online 后重新拨打，当前来电不会补发。不要在尚未定位 TLS 失败时，把同时启用三条地址的一次成功当作 TLS 成功。

## 新增会话诊断

本次更新后的 Mac 后端将记录有限的阶段/原因代码及 call UUID，不输出 SDP、ICE 地址、凭据或原始错误内容。需要结束当前通话后，在原终端重启新构建的 Go 后端才能生效；启动参数沿用原命令。

```sh
./dist/djonehub-macos --listen 127.0.0.1:7575 \
  --remote-online-profile home-xiaomi \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"

curl -fsS http://127.0.0.1:7575/api/devices/dji-75d81d083dca/remote-calls/status
```

status 新增 `session`：包含最近或当前会话的 state、end_reason、revision、call_uuid、generation、cleanup_failed；未创建 Session 时为 null。`last_error` 仍只表示路由配置读取错误。

| 原因 | 含义 |
| --- | --- |
| hub_offline_or_busy | 本通未取得空闲 Hub 连接 |
| ice_fetch_failed / ice_config_invalid | 认证 ICE API 请求或凭据校验失败，尚未发邀请 |
| relay_target_unavailable | 目标安装身份未在线或无法路由 |
| peer_closed | 对端信令连接结束 |
| local_offer_failed / local_offer_binding_invalid | Hub 生成 SDP offer 失败或 offer 绑定不匹配 |
| sdp_answer_invalid | Hub 拒绝手机 SDP answer 或其中已排队的 ICE 候选 |
| remote_ice_invalid / local_ice_binding_invalid | Hub 添加远端候选失败或本地候选绑定不匹配 |
| signaling_closed / signaling_write_failed / delivery_timeout | Hub 信令连接结束、发送失败或邀请送达确认超时 |
| transport_unavailable | 接听请求之后，ICE/DTLS 未在预算内就绪 |
| answer_failed / answer_timeout | 蜂窝接听或模块/native 宿主就绪阶段未完成 |
| audio_timeout | 已到 awaiting_audio，但 iPhone 系统音频激活没有按时确认 |
| ring_timeout | 振铃期限内未收到可处理的接听意图 |
| cancelled | 本通上下文已结束；结合主叫/对端状态判断 |

新诊断通过真实 TLS Relay 的离线目标回归及受影响包竞态测试；没有因此完成公网 iPhone 双向音频验收。下一次复现同时记录 CallKit 是否出现、接听后的结果及 session.end_reason，才可准确定位媒体阶段。

## iPhone 显示“通话已结束或连接不可用”并产生 peer_closed

该文字是旧 App 的通用状态机失败提示，并非唯一根因。Hub 的 `peer_closed` 表示 Relay 已失去该 iPhone 信令连接；可能是 App 主动结束，也可能是连接/服务端关闭。若 Hub Session 从 revision 1 的 offered 直接结束为 revision 2，符合还未收到接听命令的阶段，不能据此判断 UAC 双向音频有问题。

本次已在原生 App 增加“本次联调诊断”区域，并将状态机失败细分为固定原因代码。最新源码需要在 Xcode 中重新 Run 到接听 iPhone 才生效；只重启 Go 后端无法更新手机代码。

检查还发现原 App 只声明 `audio` 后台模式，缺少 `voip`。已在 Info.plist 与 project.yml 同步补齐；Apple CallKit 存在专门的 [missingVoIPBackgroundMode 错误](https://developer.apple.com/documentation/callkit/cxerror/code/missingvoipbackgroundmode)。这是一项需要修复的配置遗漏；旧 App 没有报告具体错误域，尚不能确认它就是本通失败的原因。此声明不等于实现 APNs/PushKit 后台唤醒。

本机确认安装身份为 `com.oops.djonehub`。打开 `ios/DJOneHubPhone/DJOneHubPhone.xcodeproj`，保持用户原 Team 和该 Bundle ID，选择 DJOneHubPhone scheme、接听用真机，Run 覆盖安装。工程签名设置未被本次修改。再次连接 App、保持前台，发起新来电，复制“本次联调诊断”中的全部文本。

诊断保留最近 24 条阶段，含本通 UUID、CallKit 报告结果、ICE 获取/校验、SDP answer、transport connected、接听动作和系统音频激活。它不记录电话号码、SDP、ICE 地址/密码、token、原始 HTTP 响应或 NSError.userInfo。网络失败保留 HTTP 状态或有限的原生错误数字，不把凭据相关的完整错误描述展示出来。

| 手机诊断 | 对应阶段 |
| --- | --- |
| report_failed_callkit_general_3 | App 缺少 VoIP 后台模式；检查是否运行最新安装包 |
| report_failed_callkit_general_1 / report_failed_callkit_incoming_1 | 系统拒绝 App 的来电报告权限 |
| report_failed_callkit_incoming_2 | 系统已有相同 UUID 的通话 |
| report_failed_callkit_incoming_3 | 系统按勿扰/专注模式过滤来电 |
| report_failed_callkit_incoming_4 | 系统按阻止名单过滤来电 |
| invalid_message / invalid_binding | 入站信令字段或身份绑定未通过验证 |
| ice_fetch_http_401 / ice_fetch_http_403 | ICE API 鉴权/模块权限失败 |
| ice_fetch_url_-1001 | ICE API 请求超时 |
| media_config_audio_... / media_config_native_... | 原生音频或 Peer 初始化失败 |
| sdp_answer_native_... | SDP 设置/生成失败 |
| transport_failed | 原生 WebRTC transport 失败 |
| transport_timeout | 用户已请求接听，但 ICE/DTLS 未在既定预算内就绪 |
| hub_answer_timeout | 已发接听命令，但未等到 Hub 的 awaiting_audio |
| audio_activation_timeout | Hub 已就绪，系统音频激活未按时确认 |
| ring_timeout / end_timeout | 振铃或结束动作超时 |

CallKit 诊断同时区分通用错误域 CXErrorDomain 与来电错误域 CXErrorDomainIncomingCall；相同数字在不同域含义不同。阶段码仍需结合前面的记录判断。`callkit_reported`、`answer_sdp_sent` 和 `transport_connected` 是不同的成功条件。PhoneCore 的 15 项测试与 3 项原生模拟器测试已通过，后者包含真实 WebRTC 协商、错误诊断隐私与 CallKit 错误域区分。真机 SDK 无签名构建通过，产物已检查包含 `audio`、`voip` 且 Bundle ID 保持 `com.oops.djonehub`。尚未用新 App 完成该用户真机的复现。

## CallKit 出现，但点击接听后回到“在线，等待来电”

该状态通常表示 App 收到了 Hub 的 ended 状态或 Relay 的 peer_closed，并保留手机信令连接；它不代表蜂窝 ATA 已成功执行。此次读取到的 Hub Session 为 offered 后直接 ended（revision 2），end_reason 为 cancelled，尚无接听状态转换证据。

旧 Hub 在 SDP offer 生成、SDP answer 接受、ICE 候选添加以及信令发送失败时，均可能通过统一 Cancel 路径留下 cancelled。现已记录上表中的固定阶段码；保留正常主叫结束/上下文取消的 cancelled，且不记录 SDP、候选地址或原始错误。真实 TLS Relay 回归分别验证 offer、answer、candidate 三类失败保留阶段码且没有 ATA/蜂窝清理操作，受影响 Go 包竞态测试通过。

先保留手机“本次联调诊断”的全部文本，与同一 UUID 的 Hub session 对照。需要重现时使用最新 Go 二进制及原启动参数，并保持 App 前台在线；不要清理现有配对。若手机记录已包含 answer_sdp_sent，但还没有 transport_connected，需要继续区分 Hub SDP/候选处理失败与公网 TURN 连通问题。

### 18:24 通话日志与后续网络对比

该 UUID 的手机记录已包含 callkit_reported、ice_config_validated、answer_sdp_sent、answer_requested，随后 received_peer_closed，没有 transport_connected。Go 对应记录为 reason cancelled / error cancelled；蜂窝记录约 15 秒后以未接听状态结束。此记录说明上下文取消，不能把早先另一 UUID 的 TURN 关闭错误当成本通根因。

后续 Mac 默认路由和公网 TURN IP 的路由仍为 utun10，DNS 为 198.18.206.233。除了默认路由探测，还通过 Darwin SDK 的 IP_BOUND_IF（25）仅把诊断 socket 绑定到 en0，连接真实 IP 47.108.184.34；没有修改 Surge 或系统路由。物理网卡对比结果：

- 443/TCP 的 HTTPS TLS 证书验证成功，GET /healthz 返回 200。
- 3478/TCP、5349/TCP 在 connect 阶段各 5 秒超时。
- 3478/UDP 的无凭据 STUN Binding 请求 5 秒无响应（该检查不是认证 TURN 分配验收）。

这表明该 Mac 物理网卡到 TURN 公网端口的路径不可用，不能仅靠 App/Go 重启恢复。下一步读取服务器 djonehub-turn 服务状态、3478/5349 监听、服务日志；再检查实际 firewalld 活跃 zone 和绑定该实例的云安全组。按实际配置放行 3478/UDP、3478/TCP、5349/TCP，以及 UDP relay 范围 49160–49259。服务器本机通、Mac 不通时继续排查公网端口路径；服务未监听时先修复 coturn 启动错误。仍需一次主叫保持等待的新通话，确认 transport_connected 后才继续模块/native 和双向音频验收。

### 18:53 服务端输出：监听正常，旧配置兼容性提示

用户提供的 systemd/ss 输出确认 djonehub-turn 正在运行，coturn 4.18.0 在 172.23.43.204:3478（TCP/UDP）和 :5349（TCP）监听。日志找到证书/私钥并初始化 TLS；这还不能代替实际 TLS 握手与证书链验证。绑定服务器实际内网地址是正常的，保留公网/内网 external-ip 映射，不必改为直接绑定公网 IP。

服务器本机先执行：

```sh
timeout 8 openssl s_client -connect 172.23.43.204:5349 \
  -servername oops.work -verify_hostname oops.work -verify_return_error </dev/null
sudo firewall-cmd --state
sudo firewall-cmd --get-active-zones
```

本机 TLS 通过、Mac 物理网卡公网 TCP connect 仍超时时，优先检查实例实际绑定的云安全组、本机防火墙与公网映射。云安全组入方向允许规则如下；阿里云端口范围写法参考[官方规则说明](https://help.aliyun.com/zh/ecs/user-guide/security-group-rules/)。远程 iPhone 地址会变化，公网联调来源可用 0.0.0.0/0，后续按实际网络范围收窄。

| 协议 | 阿里云端口范围 | 用途 |
| --- | --- | --- |
| TCP | 3478/3478 | TURN/TCP |
| UDP | 3478/3478 | TURN/UDP |
| TCP | 5349/5349 | TURN/TLS |
| UDP | 49160/49259 | 媒体 relay 范围 |

firewalld 运行时，使用上面 get-active-zones 中服务器网卡所属 zone，参照[快速部署指南](RELAY_CENTOS8_NGINX_QUICKSTART.md)给该 zone 添加当前与永久端口规则；不要为本次测试停止整个防火墙。未运行时按现有 nftables/iptables 或其他方案检查，不能视作云安全组已放行。

配置警告是另一项已修复的模板兼容性问题。依据[coturn 4.18 发布说明](https://github.com/coturn/coturn/releases/tag/4.18.0)及[该版本配置](https://github.com/coturn/coturn/blob/4.18.0/examples/etc/turnserver.conf)，CLI/DTLS 默认关闭、TLS 最低版本默认 1.2；旧 no-cli、no-dtls、no-tlsv1、no-tlsv1_1 不再需要。安装脚本现读取实际 turnserver --help，仅为仍支持的旧版本追加这些禁用项。配置渲染、安装工具链、配对脚本共 20 项测试通过，bash 语法检查通过；未在该远端服务器执行改动。

当前已知为 4.18.0 的服务器可在无通话时清理旧项，保留凭据与其他配置：

```sh
sudo cp -p /etc/djonehub/turnserver.conf \
  "/etc/djonehub/turnserver.conf.bak-$(date +%Y%m%d-%H%M%S)"
sudo sed -i -E '/^[[:space:]]*(no-cli|no-dtls|no-tlsv1|no-tlsv1_1)[[:space:]]*(#.*)?$/d' \
  /etc/djonehub/turnserver.conf
sudo systemctl restart djonehub-turn
```

这仅清理兼容性提示，不能代替公网端口放行；无需因此清理配对、修改共享 secret 或重装 App。

### 后续复查：本机 TLS 与公网入口均恢复

服务器输出确认连接 172.23.43.204:5349 时 TLS 1.3 握手成功，域名 oops.work 验证成功、Verify return code 为 0；firewall-cmd 显示 firewalld 未运行。随后 Mac 复查得到不同于早先超时的结果：

- en0 物理网卡直连 47.108.184.34 的 TCP 3478、5349 均约 0.01 秒连接成功。
- 公网 5349 的 TLS 1.3 握手与 oops.work 证书验证通过。
- 默认路由和 en0 的 3478/UDP STUN Binding 均收到 0x0101 成功响应，transaction ID 与 cookie 匹配。

公网入口已恢复可达，不能继续沿用早先超时结果判断当前仍被阻断；未知期间具体修改了哪项网络规则。上述检查仍不等于已完成认证 TURN 分配、relay 媒体端口范围、iPhone↔Hub ICE/DTLS 或双向音频验收。当前 Hub 信令 online，需让 App 前台重新连接、发起新 UUID 的来电，在 CallKit 接听后保持主叫等待，观察 transport_connected、Hub answering/awaiting_audio/connected 和 audio_session_activated。若仍失败，保留本次手机诊断和同 UUID Hub Session，不能把之前结束的 peer_closed 会话当成新通话结果。

### 新通话仍停在 answer_requested：独立认证中继诊断

6eb227e6-248d-4bb5-9be3-8117dc144cae 的手机记录到 answer_sdp_sent / answer_requested，仍没有 transport_connected。用户确认接听不了后主叫挂断，Hub 的 reason cancelled / error cancelled 与该结束动作一致，不能据此推断 SDP 错误。

新增命令用于把中继连通性从真实电话和 CallKit 动作中拆开验证：

```sh
./dist/djonehub-macos remote media-check \
  --profile home-xiaomi --module dji-75d81d083dca
```

命令需要 remoteopus 构建。它使用现有 profile 获取已鉴权的模块 ICE 配置，对每个配置 URL 单独建立两端强制 relay 的 Pion peer，交换 trickle candidate 并验证两端均选中 relay candidate pair，分别输出 udp/tcp/tls 的固定阶段、候选计数和连接状态。每 URL 媒体预算 8 秒。没有拨号、没有原生音频，也不创建额外 Hub WSS，因此可与常驻 Hub 同时运行。输出不含凭据、SDP、候选地址，库日志也被禁用。

- 两端 relay_candidates 都为 0：优先查认证分配/实际 DNS 与 TURN 连接路径。
- 已有 relay_candidates，但未 connected：继续检查返回的公网地址、UDP relay 端口范围和服务器间 relay 流量。
- connected 且 selected_relay_pair 为 true：该 URL 的 Mac↔TURN↔Mac ICE/DTLS 验证通过；仍需验证 iPhone 到 TURN 的路径、原生 SDK 互操作和双向实际音频。

真实 TLS Relay 的凭据路径保留既有模块范围校验和 profile 锁；本地真实 TURN UDP/TCP 测试验证正确 proof 可以建立 relay pair，错误 proof 无候选且不能通过。受影响包竞态测试与不带 remoteopus 的 CLI 参数检查通过。macOS 首次运行新构建可能要求系统钥匙串授权；在系统弹窗输入密码，勿把密码或凭据贴入日志/聊天。首次公网运行仍在等待该交互，未宣称公网认证中继已通过。

同时将 macOS 构建产物改为原子替换，避免 cp 覆盖旧可执行文件 inode 后，macOS 签名缓存导致下一次启动被 SIGKILL。新产物 remote --help 启动验证通过，正在运行的 Hub 保持 online。

## 公网中继测试全通过，但 iPhone 接听后闪退

用户执行 media-check 得到 UDP/TCP/TLS 三项 connected=true、relay_candidates=[1,1]、selected_relay_pair=true。这验证 Mac 使用实际 Relay 签发凭据，可以经 coturn 分别建立三种传输的强制中继 ICE/DTLS；仍不等于 iPhone 路径或真实双向音频已通过。

新增 iOS SDK↔Pion 本机互操作夹具与原生模拟器测试，已实测成功建连约 0.2 秒，且音频仍保持 CallKit 激活门控。手动重跑方法（两个终端）：

```sh
DJONEHUB_IOS_INTEROP=1 go test -tags remoteopus ./internal/remotemedia \
  -run '^TestIOSNativeInteropFixture$' -count=1 -v -timeout=4m

xcodebuild -project ios/DJOneHubPhone/DJOneHubPhone.xcodeproj \
  -scheme DJOneHubPhone -destination 'platform=iOS Simulator,name=iPhone 18 Pro' \
  CODE_SIGNING_ALLOWED=NO test
```

夹具只监听 127.0.0.1:39096，交换无身份/无 TURN 凭据的本机测试 SDP。没有夹具时，原生互操作测试会跳过，其余原生测试继续执行。

### 已定位的真机闪退

2026-10-09 连接真机后，通过 devicectl 的 systemCrashLogs 域只读取得 19:03、19:10（两份）、19:26 四份 DJOneHubPhone .ips。四份均为 EXC_BREAKPOINT/SIGTRAP，触发线程 com.apple.NSURLSession-delegate，栈为 _assertionFailure → CheckedContinuation.resume(throwing:) → SignalSocket.ping() 的 sendPing completion。不是 WebRTC 原生栈，也不是 Jetsam 内存终止。

原实现直接在每次 ping completion 中恢复同一 CheckedContinuation；已改为每次 ping 独立的 PingCompletion，NSLock 下取出并清空 continuation，解锁后仅恢复一次。心跳成功之后的取消/错误回调或并发重复回调不能再次恢复，也不会影响新的 ping。回归包含成功 pong 后再收到错误，以及首个错误后并发回调仍保留原错误。此次原生测试 6 项通过、1 项互操作夹具测试因未启动而跳过；互操作此前单独通过。Go 媒体、会话、Mac 包竞态测试通过。

修复版已沿用 Team 8AHAFH9VTZ 和 Bundle ID com.oops.djonehub 完成签名构建，并通过 devicectl 覆盖安装到连接中的 iPhone，保留原配对、未卸载。已启动 App 并采集实时控制台。尚需一次更新后的真实来电确认不再闪退和接听/音频结果。

### 候选与连接阶段诊断

手机新增有限类型的 local_ice_relay/remote_ice_relay、ICE gathering/connection 与 peer connection 数字状态，以及候选数量；不输出候选地址、SDP 或凭据。SDK 数值：ice_state 0=new、1=checking、2=connected、3=completed、4=failed、5=disconnected、6=closed；peer_state 0=new、1=connecting、2=connected、3=disconnected、4=failed、5=closed。ICE connected 还不能代表 DTLS 已 connected，仍以 transport_connected 为接听前媒体门槛。

新版 Hub status 的 session.media 包含 local_candidates、local_relay_candidates、remote_trickle_candidates、remote_relay_candidates、answer_accepted，以及 peer_state/ice_state/gathering_state。终止后保留关闭前最后状态用于诊断；remote_trickle_candidates 仅统计显式 AddCandidate 接收/排队成功的候选，SDP 内嵌候选不包含在该计数中。先用同 UUID 对照手机/Hub 两侧，确认手机有 relay、Hub 收到了 relay 和 answer，再检查候选对能否连通。

### 接听后无声并以 answer_timeout 结束：UAC 回调周期

2026-10-09 的 c993ae4c-e0ac-4b4f-8325-ff621997bc0d 会话收到接听命令后以 answer_timeout 结束。所连接 Xiaomi 模块的两个 CoreAudio 端点在 8 kHz 下均使用 512 帧缓冲，即 64 ms，超过远程音频宿主要求的 20 ms。旧宿主只检查周期，没有主动调整，导致原生音频无法完成就绪。

远程模式现在在创建 IOProc 前请求 128 帧缓冲（16 ms），确认实际周期不超过 20 ms 后启动；停止或启动失败时恢复原采样率与缓冲大小，恢复失败仍保留清理状态。普通本地/AI 模式继续使用原有启动入口。宿主新增错误日志，避免启动错误随后被空闲状态覆盖而无法定位。

实机端点配置测试确认两端 512→128 帧成功，随后均恢复 512 帧；release 构建及 8 项 Swift 测试通过。此检查没有启动音频 IO，也不代表双向通话已验收。需要重启 AudioHost 加载新二进制，再用新来电确认 answering→awaiting_audio→connected，并由两端分别说话验证声音。

Go 后端若提示 `native control file is already in use`，先检查是否已有后端运行；同一控制文件只允许一个后端持有。本次已有后端在线时，仅在原 AudioHost 终端按 Ctrl+C 后执行：

```sh
./macos/DJOneHubAudioHost/.build/release/DJOneHubAudioHost \
  --base-url http://127.0.0.1:7575 \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

在 macOS 上，替换磁盘上的可执行文件不会更新已运行的旧进程；用进程启动时间确认宿主确已重启。
