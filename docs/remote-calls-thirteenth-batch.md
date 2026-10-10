# 远程通话：第十三批 APNs/PushKit 与离线来电

日期：2026-10-09。分支：`codex/remote-iphone-callkit`。

本批在已提交的 iPhone 接听/拨号实现之上，接入 Relay APNs 发送端、安装身份推送地址注册、短期离线邀请，以及 App PushKit 唤醒。保留主动拨号和既有 AI/远程路由隔离。**代码及本机测试不等于真实 APNs、锁屏或公网验收；当前仍是联调版本。**

## 服务端行为

- `serve --apns-config /absolute/private/apns.json` 可选启用；未配置时保持前台 WSS 在线接听。`/healthz` 的 `voip_push` 表示该 Relay 是否装载 APNs 配置，不表示 Apple 已接受密钥。
- 新增 `POST /v1/push`，安装 access token 鉴权，body 为 `{"environment":"sandbox","token":"小写十六进制地址"}`。只允许安装身份修改自己的地址；传空 token 删除。环境必须与 Relay 相同，不接受客户端指定 topic、Team、Hub、目标安装或 APNs URL。
- SQLite schema 从 1 升到 2，保留原配对和凭据，增加私有推送地址表。轮换用注册版本和 APNs 时间戳保护，旧失败回执不能删除新注册。已撤销或 refresh 已到期的身份不能成为离线推送目标。**升级前备份数据库；旧二进制不能直接打开 schema 2。**
- APNs 使用固定 Apple HTTPS 地址、HTTP/2、ES256 provider token，20 分钟内复用签名；topic 为 `<bundle-id>.voip`，priority 10，expiration 0。密钥只保留在 Relay，消息只携带通话邀请，不携带 token、私钥、SDP 或 TURN 密码。单次请求最长 5 秒，且不超过振铃期限。
- 每个 Hub 只有一个活动/待唤醒路由，整个 Relay 最多 32 个唤醒任务。手机在线时沿用 WSS，不另外发送 Push；离线时向 Hub 返回 `waking`，最多等待本通原有振铃期限。仅在手机重新鉴权、收到待处理邀请之后才返回 `delivered`，Hub 才发送 offer。两种回执都不是 CallKit 成功或接听确认。
- APNs 超时、服务错误不自动重试，避免“不知道 Apple 是否已接收”的重复来电。BadDeviceToken、DeviceTokenNotForTopic 与合适时间戳的 Unregistered 作废对应注册；不因暂时失败清除地址。
- 挂断/Hub 断线、振铃超时或 Relay 重启结束待处理邀请，不持久化和重放。新 Hub epoch 不能恢复旧电话；仅允许从未连接过本通的目标手机接收待处理邀请，不恢复已断开的活动通话。已有 AI 分支不会进入此流程。

Hub `/api/remote-calls/status` 的 `capabilities.voip_push` 是本地协议支持，`push_configuration_verified:false` 表示未检查远端 APNs 配置；总 `available` 仍为 false，原因 `remote_validation_pending`。实际配置能力以 Relay `/healthz` 和 App 注册状态为准。

## iOS 生命周期

1. 沿用原 Team、Bundle ID 和 Keychain 身份覆盖安装，避免卸载。用户在前台授予麦克风权限并点击“启用后台来电”；推送地址通过安装身份提交 Relay。默认不启用后台推送。
2. `UIApplicationDelegate` 在启动时恢复已启用的 PushKit 注册，使用主队列。令牌变化、失效和重新连接后重新同步；关闭功能先删除服务端地址，失败时提示重试或撤销身份。
3. 收到 VoIP Push 后，在回调中立即调用 `reportNewIncomingCall`，然后才读取身份、检查截止时间并连接 WSS；在 CallKit completion 中调用 PushKit completion。即使推送已过期或格式不合法，也先报告后结束，不等待网络查询再报告。
4. Push 只是唤醒提示。只有持有原 Keychain 身份、通过 WSS 鉴权并匹配全部通话绑定与邀请内容，才能接入现有状态机。不能由 Push 指定 Relay 地址、授权 ATA/ATH 或开启音频。
5. 用户抢先点击接听时暂存 CallKit 动作，最多等 12 秒恢复邀请；仍需 DTLS、Hub 蜂窝/宿主就绪后才 fulfill，系统音频激活后才开启媒体。抢先拒接时先结束系统动作，再在鉴权恢复后发送一次 reject；恢复失败时不发送未经鉴权的模块命令。
6. 推送和 WSS 先后顺序可能不同；已报告的同一绑定交接给状态机，不重复创建 CallKit 通话。迟到、错误身份、过期或无法恢复的提示结束 UI。已在途的推送不能撤回，可能短暂出现已经失效的来电界面。
7. 后台通话结束后主动关闭空闲 WSS，下一通由 Push 唤醒。主动拨号仍需前台连接。操作系统限制、用户强制退出、重启后首次解锁前 Keychain 不可用等场景必须真机实测，不能承诺必达。

实现依据：[Apple PushKit 来电处理](https://developer.apple.com/documentation/pushkit/responding-to-voip-notifications-from-pushkit)、[Apple APNs 请求规范](https://developer.apple.com/documentation/usernotifications/sending-notification-requests-to-apns)。当前选择每通只发送一次，收敛了设计基线中可选的有限重试策略。

## 配置与部署

现有 CentOS/Nginx 部署可使用已准备的[升级包与执行步骤](REMOTE_APNS_UPGRADE.md)，无需重建配对。

公网域名和 Relay 地址沿用现有部署文档，无需重新配对。以下示例不是已执行的生产变更。

1. 在 Apple Developer 为实际 App ID 启用 Push Notifications，配置有权限的 APNs signing key 和签名 profile。不要改变已配对 App 的 Bundle ID。
2. 使用下面的生成脚本（需要 Python 3 和 OpenSSL），或参考 `deploy/remote-call/apns.example.json`，填入 Team ID、Key ID、实际 Bundle ID、环境及 PKCS#8 `.p8` 内容（JSON 中的 `private_key_pem` 字符串）。文件必须为运行 Relay 的用户拥有、权限 0600 的普通文件，不允许符号链接/多硬链接；绝对路径，最大 8 KiB。不要把填好的文件提交到 Git 或把私钥写到命令行。
3. 在原 Relay 启动参数增加 `--apns-config /etc/djonehub/apns.json`，保留原 `--state-dir`、TLS 和 TURN 配置；先备份数据库再升级。服务需要向对应 Apple APNs HTTPS 443 出站，不能用普通 HTTP 代理改写请求。
4. Xcode 项目 Debug 默认 `aps-environment=development` / API 环境 `sandbox`；Release 默认两者为 `production`。XcodeGen 可重新生成配置；自定义签名必须核对**最终签名 entitlement**与 Relay 环境一致。一个 Relay 实例当前对应一个 Bundle ID 和一种 APNs 环境，不混用开发版与发布版。
5. 安装后在前台启用后台来电，确认 App 显示注册成功。关闭前台连接、锁屏，再从真实电话拨入。APNs 200 或注册成功本身不能作为通话通过证据。

### 从 Apple 下载密钥并生成 JSON

进入 Apple Developer → Certificates, Identifiers & Profiles → Keys → 新建，启用 Apple Push Notification service，选择与 App 签名匹配的环境及 key 类型，下载 `.p8` 并记录 Key ID。Team ID 来自会员信息，Bundle ID 使用 Xcode 中当前 App 的值。需要 Account Holder/Admin 权限；私钥只能下载一次。[Apple 创建私钥说明](https://developer.apple.com/help/account/keys/create-a-private-key/)

新 APNs key 可能限定环境或 topic，不能假定一把新 key 同时支持 sandbox 和 production；topic 限定的 key 必须允许实际 VoIP topic。[Apple token 鉴权说明](https://developer.apple.com/documentation/usernotifications/establishing-a-token-based-connection-to-apns)

```sh
mkdir -p "$HOME/.config/djonehub"
chmod 700 "$HOME/.config/djonehub"
python3 scripts/create-apns-config.py \
  --p8 "$HOME/Downloads/AuthKey_0123456789.p8" \
  --team-id ABCDEFGHIJ \
  --key-id 0123456789 \
  --bundle-id org.djonehub.phone \
  --environment sandbox \
  --out "$HOME/.config/djonehub/apns.json"
```

以上 ID 和文件名均为占位示例，需替换。脚本验证 P-256 key、公钥类型、基本字段，生成 0600 JSON，拒绝覆盖已有文件和符号链接，不打印密钥。它不向 Apple 申请密钥，也不能验证该 key 的实际推送权限。App 使用 Xcode Debug 真机安装时通常选择 sandbox，TestFlight/发布版选择 production，以最终签名 entitlement 为准。

生成结构为：

```json
{
  "team_id": "ABCDEFGHIJ",
  "key_id": "0123456789",
  "bundle_id": "org.djonehub.phone",
  "environment": "sandbox",
  "private_key_pem": "-----BEGIN PRIVATE KEY-----\n…由脚本写入…\n-----END PRIVATE KEY-----\n"
}
```

配置只提供给 Relay，iPhone 和 Mac Hub 不需要持有 `.p8`。交付配置路径即可，不要在聊天中粘贴私钥。

## 验证与剩余工作

本机覆盖：真实 TLS HTTP/2 的 APNs 请求格式和 ES256 签名、私有配置文件、schema 1→2 迁移、租户/模块/撤销隔离、令牌轮换、旧失败回执、离线邀请恢复和过期、Hub 断线、原 5 秒在线超时之后手机再连接、推送提示不能直接授权接听。全仓库 Go 回归及受影响包竞态检查通过，保留 AI 路由、主动拨号、Opus 与 TURN 回归。

Swift Core 共 21 项测试通过，包含推送解析/绑定、无 WSS 时不能接听或开音频、注册鉴权与失效测试。真机 SDK 无签名构建通过；模拟器原生测试 7 项全部通过，包含配套 Go/Pion 互通（本次无跳过项）；该测试不能证明 Apple 推送投递或真实 CallKit 后台行为。

真机验收至少包括：开发和发布环境分别投递、锁屏/后台/冷启动、重复/迟到 Push、来电方在唤醒前挂断、用户在 WSS 恢复前接听/拒接、令牌更新、撤销安装、凭据刷新、来电连续两次、Wi-Fi/蜂窝与强制 TURN、蓝牙/系统电话打断。不要用 VoIP Push 发送测试提示、取消消息或非来电通知；真实推送应由有效的电话来电触发。

尚未实现通话中网络切换的 ICE restart、长通话漂移/耗电验证和公开分发验收；这些继续属于总计划的后续工作。
