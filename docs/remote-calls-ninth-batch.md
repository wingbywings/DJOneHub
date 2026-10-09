# 远程接听：第九批活动连接续期与凭据刷新客户端

日期：2026-10-02。分支：`codex/remote-iphone-callkit`。

本批解决第八批信令连接只能使用最初访问凭据的问题：增加同一身份在活动 WSS 上续期、Go 自动刷新调度器，以及注册/配对/刷新 HTTP 客户端。通过测试验证续期保留原通话路由和已连接会话状态，旧凭据到期不会取消已经成功续期的连接。

**这仍不是 iPhone 真机或长通话交付。** 目前没有已接入的 Keychain/受保护凭据存储、Mac 配对界面和常驻 Hub 连接管理；APNs、iOS App、TURN、网络恢复与 3 小时长通话实测也尚未完成。正式来电自动分发继续关闭，Mac 状态仍不可用。

## 活动 WSS 续期

welcome payload 增加 `access_expires_at`，由 Relay 返回当前访问凭据的实际到期时间。客户端刷新得到新访问凭据后，在同一个 socket 上发送：

```json
{
  "version": 1,
  "type": "reauth",
  "request_id": "unique-renewal-id",
  "payload": {"access_token":"NEW_ACCESS_TOKEN"}
}
```

该消息是连接级操作，不携带 Hub epoch、模块、通话 UUID、generation 或安装 ID；协议拒绝把凭据混入通话路由消息。刷新凭据和私钥不会通过 WSS 传递。

Relay 在仍存活且旧访问凭据尚有效的连接上核对新凭据，要求以下字段全部一致：身份 ID、角色、租户、所属 Hub、身份版本和模块范围。不能通过续期替换成另一个 Hub/安装、提升权限，或复活已经过期、撤销、断开的连接。失败关闭 socket，不转发或回显带凭据的消息。

成功响应保留原 request ID：

```json
{
  "version": 1,
  "type": "reauthenticated",
  "request_id": "unique-renewal-id",
  "payload": {"access_expires_at":"2026-10-02T00:15:00Z"}
}
```

服务端用受锁保护的操作替换访问凭据和授权监视器。消息鉴权与替换串行化；旧监视器迟到的取消不能结束新授权。连接 epoch、双方连接对象、通话映射、request ID 去重和媒体 generation 均不改变。续期后的撤销仍关闭连接并触发既有会话清理。

客户端 `callsignal.Client.Renew` 串行执行续期，将确认消息从普通通话消息流中单独处理。确认超时（5 秒）或等待期间取消会关闭连接，不猜测服务端当前持有哪个 token。该超时策略与网络恢复仍需结合真机体验继续验证。

## 自动续期与会话接入

`Client.MaintainAuthorization(ctx, refresh)` 按服务端到期时间提前 1 分钟调用刷新函数；刷新和 WSS 确认共用最多 30 秒且不超过旧凭据到期时刻的预算。同一客户端只运行一个维护循环，停止时关闭对应连接。刷新函数必须遵守 context。

- `refresh` 返回新访问凭据之前必须已经安全保存刷新结果。
- 保存失败、刷新失败、续期失败或服务端返回不足以继续使用的到期时间，结束该连接。
- 已过期访问凭据不能续接旧 socket，但仍有效的刷新凭据可以在打开新 socket 之前获取新访问凭据。
- 自动刷新依赖系统时钟；本批没有处理显著跨设备时钟偏移，也没有提供弱网无限重试。

`remotecall.SignalCall.RefreshAccess` 接受该回调，`RunSignaledCall` 在通话期间运行维护循环，并在结束时取消、等待维护工作退出。Mac 的 `runRemoteSignaling` 也接受此回调；未来连接管理器应传入下述 `Credentials.RefreshAccess`。AI 路由在创建会话和启动刷新之前退出。

若调用方没有提供刷新回调，连接仍在最初访问凭据到期时关闭。正式启用时不能遗漏该集成，也不能仅凭 API 存在就宣称长通话已可用。

## Go 身份客户端

新增 `internal/relayclient`：

| 方法 | 职责 |
| --- | --- |
| `New(baseURL, options)` | 固定 Relay origin；默认 HTTPS，显式开发选项仅允许字面量 loopback HTTP |
| `Client.Enroll` | 用一次性 Hub 注册凭据和 Ed25519 私钥构造签名并注册 |
| `Client.Pair` | 用一次性配对凭据和安装私钥构造签名并兑换 |
| `NewCredentials` | 从调用方加载的身份、凭据和私钥建立刷新管理器，要求提供保存回调 |
| `Credentials.RefreshAccess` | 串行申请 challenge、签名刷新、校验身份不变、保存完整新凭据，随后返回新访问 token |

HTTP 客户端不使用环境代理、不跟随重定向；拒绝 userinfo、query、fragment、非根路径和禁用证书校验。请求总预算 10 秒，TLS 握手及响应头预算各 5 秒，响应体最多 32 KiB，严格解析已知 JSON 字段。错误返回固定类别，不携带服务端响应正文、签名、token 或私钥。

首次注册/配对返回的凭据仍由调用方负责立即保存；私钥需要在兑换之前保存在受保护存储中。本批没有新增存放明文凭据的默认文件，也没有调用系统 Keychain。

### 刷新保存与失败边界

刷新管理器持有独立的私钥和模块范围副本，用互斥锁串行化整个轮换与保存过程；并发调用不会同时使用同一旧刷新凭据。服务端响应必须仍属于同一身份、角色、Hub、租户、版本和范围，而且刷新 token 必须已经变化。

保存回调必须原子且持久地写入完整 `identity.Tokens`，在返回成功之前不能仅更新缓存。只有回调成功，管理器才更新内部凭据并向 WSS 返回新访问 token。测试中的回调只验证调用顺序和失败行为，不等于生产凭据存储已完成。

一旦提交了刷新请求，响应丢失、响应非法或保存失败都可能意味着旧刷新凭据已被消费。该管理器实例进入不可继续刷新状态，拒绝再次提交旧凭据，不因自动重试触发服务端的重用撤销。当前恢复方式仍是重新注册/配对；没有幂等取回轮换结果。

这个阻断状态目前只在内存中。**进程崩溃后的刷新事务日志和恢复标记尚未实现**；后续受保护存储必须一起处理“请求已发出但新凭据未落盘”的情况，不能在重启后盲目重复刷新。与第七批一样，本批没有消除网络响应丢失导致重新配对的限制。

## 验证

新增 10 项测试，并扩展第八批已连接会话测试：

- 真实 TLS WSS 上两端续期，随后令旧 token 过期，确认原路由和 Hub epoch 保留。
- 拒绝同租户另一 Hub 的 token；过期连接不能被新 token 复活；续期后的撤销仍有效。
- 将即将到期的 welcome 授权接入真实 HTTPS 刷新客户端，确认保存完成后 WSS 自动更新，再继续传递原通话命令。
- 注册/配对与四个并发刷新调用，验证没有旧刷新凭据重用，保存结果是当前凭据。
- 注入保存失败、服务端已经轮换但响应截断，验证不会再次刷新或错误宣告保存成功。
- 从已过期 access 加载仍有效 refresh；刷新开始前取消不会烧毁凭据。
- 拒绝重定向、超大响应、未知字段、不安全端点，以及带通话绑定/自报身份的 reauth。
- 已连接的 Session 在手机续期后继续接受音频状态，ATA 和媒体激活计数均保持一次；之后撤销仍执行清理。

全量 Go 测试和受影响包竞态测试通过，真实 WSS → Pion/Opus 协商测试继续通过：

```sh
go test ./...
go test -race -tags remoteopus ./internal/callsignal ./internal/relay/... ./internal/relayclient ./internal/remotecall ./cmd/djonehub-relay ./cmd/djonehub-macos ./internal/remotemedia/... ./internal/callcontrol -timeout 90s
```

验证在本机完成，未部署公网或操作真实电话。下一步是受保护凭据存储与崩溃恢复、常驻 Hub 连接管理和配对操作入口，再完成 APNs/iOS CallKit/TURN 集成及实测。

后续实现：Keychain 适配、跨进程刷新日志、重启后的保护与原生注册/配对命令已在 [第十批](remote-calls-tenth-batch.md) 实现；真实 Keychain 授权和发行签名仍需验证。
