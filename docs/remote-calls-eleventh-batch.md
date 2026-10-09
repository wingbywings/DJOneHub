# 远程接听：第十一批常驻 Hub 连接与来电调度

日期：2026-10-09。分支：`codex/remote-iphone-callkit`。

本批将现有 Keychain profile、鉴权 WSS、通话协调和 Pion/Opus 媒体接到实际来电轮询入口。通过显式的 `--remote-online-profile` 参数，可在 USB 多模块模式下启用在线联调：已配对客户端必须已经连接 Relay。

**iPhone CallKit App、APNs 唤醒与 TURN 尚未完成，不能视为已经支持 iPhone 公网接听。** 这里交付的是可运行、经过自动化验证的 Hub 调度链路。没有操作真实电话、音频硬件或用户 Keychain；真机 Keychain 签名/访问、UAC、蜂窝接听仍待验证。

## 连接与通话生命周期

- 一个进程使用一个 Hub profile、一个常驻 WSS 连接。空闲和通话期间由同一 worker 维护访问凭据，沿用第十批的持久化轮换日志与跨进程锁。
- 来电轮询发布状态后，只对不可变路由为 remote、仍在 incoming、没有控制 owner/state 且未超时的通话尝试调度。AI 即使 Provider 不可用也不会进入远程入口；waiting、去电、无模块 ID 的旧串口模式不分发。
- 每个通话 UUID 只尝试一次。离线、连接忙、主叫取消、配置目标不在线或本机尚未启动时，不排队、不在重连后重新邀请；也不回退到 AI。下一次新来电才能重新尝试。
- 每个 Hub 同时只运行一路远程会话；多个模块同时来电时只有一路可占用连接。接听时仍经过原有全 Hub 音频租约、当前 CLCC 和通话 UUID 校验，本地/AI 使用音频资源不会被远程抢占。
- 媒体 generation 固定为 1，因为同一个 UUID 不会重试。旧 generation/epoch 不会迁移到新连接。
- 连接失效、凭据续期失败、主叫取消、模块移除或退出程序时取消本通上下文。先等待既有 Session 的有限时间清理，再释放连接和续期 worker，之后才重连。
- 一通会话结束后关闭该 WSS，下一通使用新连接和新 epoch。失败重连采用指数退避，基数 1–30 秒，加最多 50% 随机抖动。消费过一次通话或连接存活至少 30 秒后重置基数，因此短通话不会不断累积退避。
- 管理器不读取活动会话的信令消息，避免与会话争抢同一个 reader；空闲时收到意外消息会关闭连接，不能由远端消息自行创建电话。

`internal/remotehub` 管理连接及调用串行化；`cmd/djonehub-macos/remote_dispatch.go` 管理原生 profile 接入和通话条件；既有 `runRemoteSignaling` 负责绑定身份、SDP/ICE、命令和 Session。

## 联调启动

准备工作：使用第十批原生命令注册一个 ready 的 Hub profile、配对客户端，并在对应模块远程配置中填入真实安装 ID。Hub 和安装身份都必须有该 `dji-...` 模块权限。必须先关闭该模块的 AI 接听，配置只能在无通话时修改。

构建需要已安装的 libopus。本批原验证版本为 1.5.2；2026-10-09 兼容性更新后，构建脚本也支持 1.6.1，当前安装步骤见 [联调指南](REMOTE_IPHONE_INSTALL_AND_TEST.md)：

```sh
DJONEHUB_REMOTE_OPUS=1 ./scripts/build-macos.sh
```

在本机私有目录启动控制服务及在线调度，例如：

```sh
mkdir -p "$HOME/Library/Application Support/DJOneHub/native-control"
chmod 700 "$HOME/Library/Application Support/DJOneHub/native-control"

./dist/djonehub-macos \
  --remote-online-profile home \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

另一个终端启动已经构建的 Swift 宿主，使用同一 bootstrap 路径：

```sh
./djonehub-audio-host --base-url http://127.0.0.1:7575 \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

要求与边界：

- 仅自动发现的 USB 多模块模式；拒绝 demo、显式串口及自动发现后的旧串口回退。启动器尚未自动传递此参数。
- 缺少 `remoteopus`、native 控制文件参数或 ready profile 时启动失败并报告原因，不自动注册身份。常驻入口只使用 HTTPS/WSS 和系统信任链，不提供公网明文或跳过证书验证开关。
- 注册、检查、常驻服务应使用同一稳定签名程序；开发二进制变化后的 Keychain 可访问性仍需实测。
- 运行期间不要再用 `remote check` 作为该 Hub 的在线探针：它会尝试建立第二条连接，被 Relay 单连接规则拒绝。可查询下面的连接状态。
- 目前 Peer 没有配置 STUN/TURN，只可用于网络可直连的联调。该参数不承诺跨 NAT、蜂窝网络或 UDP 受限网络可用。
- 原生宿主就绪和 iOS `audio_active` 门禁继续生效；在线邀请不是已接听，SDP 成功也不是可以播放音频。

## 状态接口

原有每模块 `remote-calls/status` 增加：

```json
{
  "available": false,
  "reason": "online_only_not_production",
  "online_transport": {"configured": true, "state": "online"},
  "capabilities": {"signaling": true, "voip_push": false, "remote_media": true}
}
```

`state` 可为 disabled、connecting、online、busy、retrying、stopped。`online` 只表示 Hub WSS 已建立，不代表目标 iPhone 已在线、网络可连通或 UAC 已就绪。`remote_media` 表示联调模式已配置且编译了 Opus，并不是本通音频激活状态。未启用参数时仍保留之前的不可用状态；AI 模块的 reason 仍为 `ai_route_selected`。

状态输出不包含 profile 凭据、私钥、媒体 token 或电话号码。连接错误使用有限状态表示；凭据事务不确定时仍需通过 `remote status` 和第十批恢复流程处理，不会盲目重复轮换。

## 验证

新增 9 个顶层测试（另含 6 项条件子测试），覆盖：

- AI Provider 失败时零远程调度；并发轮询只邀请一次；同一 CLCC index 替换电话号码后新建 UUID、取消旧上下文。
- 已抢接、过期、waiting、去电、无模块范围、已取消通话不进入调度。
- 32 个竞争请求不能抢占活动连接；断线后等待通话清理才重新连接；离线不排队，退出时等待 worker。
- 真实 TLS Relay 上断线重连获得新 epoch，不重发旧邀请，新通话仍能路由。
- 带 `remoteopus` 的真实应用链路：模拟 CLCC → 实际调度 → Session/Pion → 鉴权 WSS → 邀请和 Opus SDP；主叫取消后远端收到关闭通知；全程零 AT 指令。
- 状态将在线传输与完整产品可用性分别呈现。

验证命令：

```sh
go test ./...
go test -race -tags remoteopus ./internal/remotehub ./internal/relayprofile ./internal/relayclient ./internal/callsignal ./internal/relay/... ./internal/remotecall ./cmd/djonehub-macos ./cmd/djonehub-relay ./internal/remotemedia/... ./internal/callcontrol -timeout 90s
go build -tags remoteopus -o /tmp/djonehub-macos-remote-online ./cmd/djonehub-macos
```

接下来仍需实现 iOS App 的配对、CallKit/WebRTC 生命周期，APNs/PushKit、TURN 凭据及部署，以及媒体自适应、时钟补偿、网络切换和真机验收。
