# 远程接听：第八批鉴权 WSS 与 Hub 会话适配

日期：2026-10-02。分支：`codex/remote-iphone-callkit`。

本批将第七批身份接入真实 TLS WebSocket，增加在线来电邀请、受限信令转发、Go 主动连接客户端，并接到第五批会话状态机及真实 Pion Peer。**当前仍不能用 iPhone 接听实际来电：Mac 注册/配对和连接管理器、APNs、iOS App、TURN 部署尚未接入。** 新增的 Mac 内部适配入口没有注册为公开 API，也尚未由来电轮询自动调用；Mac 状态继续 `available=false`。

## 交付范围

| 入口 | 实现 |
| --- | --- |
| `internal/callsignal` | v1 消息模型、方向及大小校验、Go WSS 客户端 |
| `internal/relay/signaling.go` | 身份握手、在线路由、连接代次、撤销/断连清理 |
| `internal/remotecall/signaling.go` | 邀请、SDP/ICE、接听/拒接/挂断、音频激活、状态推送适配 |
| `cmd/djonehub-macos/remote_session.go` | `runRemoteSignaling`：可信连接到现有通话及 native UAC 就绪门禁 |
| `cmd/djonehub-relay` | 启动在线信令，停服主动关闭 WebSocket |

Relay `/healthz` 现在返回 `identity=true, signaling=true, voip_push=false`；这里的 signaling 表示在线转发 API 已实现，不代表端到端通话可用。启动方法沿用 [第七批](remote-calls-seventh-batch.md)，没有新的凭据命令行参数。

## 连接与授权

客户端主动连接 `wss://<relay>/v1/signaling`，携带 `Authorization: Bearer ...`，子协议必须为 `djonehub.calls.v1`。拒绝 Origin、query string、错误子协议和无效身份。客户端核对服务端 welcome 中的身份 ID、角色和 Hub ID，使用系统信任链或显式受信 CA，不允许关闭证书校验。

Go 客户端不使用环境代理、不跟随 HTTP 重定向。开发环境仅在显式 `AllowInsecureLoopback` 且目标为字面量 loopback IP 时允许 `ws://`；公网必须使用 WSS。实现遵循 Gorilla 的单读者、串行写入和持续读取控制帧约束，见 [官方文档](https://pkg.go.dev/github.com/gorilla/websocket)。

- 一个身份同时只允许一个连接；第二个连接返回 409，不覆盖原连接。首次握手即预留身份，避免并发 Upgrade 抢占。
- 每个连接生成随机 epoch。Hub 的 epoch 随邀请传到手机，之后所有通话消息必须携带同一个 Hub epoch。
- 每条消息重新验证发送方访问凭据和模块权限；投递前检查接收方权限，发送队列中的消息也重新验证接收方凭据。
- 只有 Hub 能邀请自己的安装身份，而且该身份必须已在线、仍有效且有该模块权限。模块范围来自数据库，不使用消息中的自报角色或租户。
- 连接复用既有撤销通知与每秒复核。撤销/到期使连接关闭，其关联路由被移除，对端收到 `peer_closed`。已在传输中的消息不提供跨进程瞬时撤回保证；Hub 收到断连后取消会话并做有界清理。
- 连接丢失、重连或服务重启均不重放旧命令。不恢复原呼叫；旧 socket、epoch、UUID 或 generation 不得选中新的通话。

本批初始版本的令牌不能在既有连接上更新；访问凭据到期会关闭连接。由于访问凭据有效期 15 分钟，**本批不能用于承诺长通话不中断**。连接鉴权续期与客户端刷新串行化已继续实现，见 [第九批活动连接续期与凭据刷新](remote-calls-ninth-batch.md)；仍需完成实际凭据存储接入、恢复策略及长通话验证，才能开放正式远程接听。

## v1 消息

除初始 welcome 外，每条通话消息均包含以下 envelope：

```json
{
  "version": 1,
  "type": "command",
  "hub_epoch": "server-issued-hub-epoch",
  "module_id": "dji-012345abcdef",
  "call_uuid": "00000000-0000-4000-8000-000000000001",
  "generation": 1,
  "installation_id": "ios_SERVER_ASSIGNED_ID",
  "request_id": "unique-request-id",
  "payload": {"action":"answer","expected_revision":1}
}
```

服务端 welcome 的 payload 为 `{id,kind,hub_id}`；Hub 使用 welcome envelope 的 `hub_epoch`，手机使用收到的 invite 的 `hub_epoch`。手机自己 welcome 中的 epoch 不能代替 Hub epoch。

| 类型 | 方向 | payload / 语义 |
| --- | --- | --- |
| `invite` | Hub → 安装 | `deadline`（RFC3339 时间）、`display`；必须在未来 90 秒内 |
| `delivered` | Relay → Hub | `accepted=true`；仅表示邀请已入在线发送队列，不表示手机响铃、CallKit 已报告或蜂窝已接通 |
| `offer` | Hub → 安装 | `type=offer, sdp` |
| `answer_sdp` | 安装 → Hub | `type=answer, sdp` |
| `ice` | 双向 | `candidate, sdpMid?, sdpMLineIndex?, usernameFragment?` |
| `command` | 安装 → Hub | `action=answer/reject/hangup, expected_revision` |
| `audio_active` | 安装 → Hub | `sequence, active`，独立单调序号 |
| `result` | Hub → 安装 | 原 request ID；`accepted, code?`，确认用户意图被接受，不代替状态完成 |
| `state` | Hub → 安装 | `revision, state, reason?`；Hub 是通话事实来源 |
| `error` | Relay → 发送者 | `code`，不回传底层 SQL、SDP 或凭据错误 |
| `peer_closed` | Relay → 剩余对端 | `code=peer_disconnected`；调用方必须终止本通远程流程 |

支持的状态是 `offered / answering / rejecting / awaiting_audio / connected / ended`。`awaiting_audio` 表示媒体传输、蜂窝与宿主已达到接听门槛；仍需 iOS 确认音频激活，才能进入 `connected`。本批尚无 CallKit 上报成功事件、DTMF、静音信令或 ICE restart。

Relay 每个 Hub 同时仅保存一通路由，绑定两端的具体连接对象及 `{epoch,module_id,call_uuid,generation,installation_id}`。同一 Hub 连接的已使用 UUID 保留为有界墓碑，结束后不能重新邀请。Relay 不判断蜂窝是否已接通，也不持久化通话或 SDP；进程重启会断开两端并丢弃路由。

邀请重试不提供“再次投递”语义；重复邀请返回错误。用户操作的 request ID 幂等仍由 Hub 会话层处理，在 revision 校验前返回重复请求结果。本批不宣称端到端 exactly-once。

## 有界资源与失败行为

- 只接受 UTF-8 JSON 文本，拒绝未知字段、非对象 payload、追加 JSON 和二进制帧。
- 单条消息最多 128 KiB；SDP 最多 64 KiB；candidate 最多 4096 字节；显示内容最多 128 字节。
- 每连接输入限流 20 条/秒、突发 40，包含 ping/pong；每通双方累计最多 256 条 ICE 消息。
- Relay 最多 128 个并发身份连接；每端发送/接收待处理队列各为 8 条。积压满时关闭连接，避免不断缓冲陈旧控制消息。
- 每个 Hub 连接最多记录 1024 个已使用 UUID；到限额需要重新连接。当前会话适配器本来就只拥有单通连接，结束后关闭。
- 握手、普通写入和等待邀请投递确认的预算各 5 秒；每 15 秒 ping，45 秒收不到 pong 关闭连接。
- 安装身份不在线时立即返回 `target_unavailable`；没有离线队列、后台唤醒或 APNs 成功假设。
- HTTP Server 停止时显式取消信令连接；不能只依赖 `net/http.Shutdown` 清理已 Upgrade 的 WebSocket。

Relay 不根据收到 `answer` 就认定接通。振铃期限及接听预算由 Hub Session 执行；Relay 路由在 Hub 终态或任一连接离开时清理。

## Hub 与媒体时序

`RunSignaledCall` 只接受 Remote 路由及其冻结的目标安装身份，AI/本地路由会在创建 Session/Peer 和发送邀请之前拒绝。调用方负责串行化同一 Hub 的通话；当前适配器拥有一条连接的读取流，结束后关闭连接，不是已完成的常驻 Hub 连接管理器。

流程如下：

1. 用现有会话工厂建立绑定同一 UUID/generation 的 Session 和 Peer，发送在线邀请。
2. 收到 Relay 的入队确认后，在可取消的 5 秒预算内生成 offer，发送 SDP/ICE；协商期间继续处理用户命令和断连。
3. 收到 `answer` 后，现有 Session 先取得接听权，再等待真实传输连接；满足条件后才调用绑定本通电话的 ATA。
4. 等待蜂窝 active、模块和受认证 native 宿主就绪；收到合法 `audio_active` 后才激活媒体。
5. 信令错误、断连或撤销取消 Session；已发送或结果不明的 ATA 沿用原有 call-bound 收尾。清理结果不确定时仍保留音频所有权，不能误操作下一通电话。

`runRemoteSignaling` 已把该适配器连接到 Mac 的 `startRemoteSession`、真实 `remotemedia.NewPeer` 和第六批 native 就绪检查。它只读取现有通话快照，没有增加接受客户端自报身份的公开接口。当前来电显示固定为 `DJOneHub`，尚未传递号码。

## 验证

新增 11 项测试（其中 1 项需要 `remoteopus`）：

- URL/TLS 策略、协议字段与发送方向。
- 真实 TLS WSS 邀请及命令转发、同租户跨 Hub/跨模块/错误安装隔离。
- UUID 墓碑、旧 generation、重连 epoch、第二连接拒绝、停服断开。
- 凭证撤销断开连接并通知对端、握手认证、Origin/query/subprotocol 和超大帧。
- WSS 到 Session 的接听幂等、传输连接前禁止 ATA、音频激活门禁、撤销收尾、接听前断连不误挂断，以及 AI 不创建远程会话。
- **真实 Pion/Opus Peer 通过 WSS 交换 SDP，完成 ICE/DTLS 后才触发模拟 ATA，断连后执行模拟蜂窝清理。** 此测试不经过真实 USB/UAC/iPhone；真实 SRTP 双向音频仍由第四批媒体回归测试覆盖。

通过全量 Go 回归、受影响包竞态测试及 Relay 构建：

```sh
go test ./...
go test -race -tags remoteopus ./internal/callsignal ./internal/relay/... ./internal/remotecall ./cmd/djonehub-relay ./cmd/djonehub-macos ./internal/remotemedia/... ./internal/callcontrol -timeout 90s
go build -o /tmp/djonehub-relay ./cmd/djonehub-relay
```

本批没有修改 Swift 或部署公网服务。下一步需要实现安全保存与刷新凭据的 Hub 连接管理、APNs 唤醒及邀请恢复，再接 iOS CallKit/Keychain、TURN、网络恢复和真机测试；这些条件满足前继续保持正式来电路由不可用。
