# DJOneHub 凭据更新指南与长期有效性评估

更新：2026-11-03。适用对象：Mac Hub 登记（enrollment）、iPhone 安装身份（installation）、配对授权（pairing grant）。Relay 为 `https://oops.work`。

## 1. 凭据体系速览（全部为代码内常量，无启动参数可调）

| 凭据 | 有效期 | 续期行为 | 过期后果 |
| --- | --- | --- | --- |
| Hub/iPhone access token | **15 分钟**（`internal/relay/identity/store.go:34`） | 常驻连接在到期前 1 分钟自动轮换（`internal/callsignal/client.go:248`）；WS 内也有 `reauthorize` 续期协议 | 请求 401 `invalid_credential`，触发重连即恢复 |
| Hub/iPhone refresh token | **30 天滑动窗口**（`store.go:35`） | 每次刷新必轮换并重置 30 天；只要 30 天内成功刷新过一次就一直有效，无绝对上限 | 401 `invalid_credential`，只能重新登记/重新配对 |
| 配对二维码 / 配对文件 | **5 分钟，一次性**（`store.go:344`） | 不可续期；到期未消费自动作废 | 重新生成新二维码 |
| 离线登记文件（enrollment grant） | **10 分钟，一次性** | 不可续期 | 重新申请 |
| Web 登录会话（本页） | 30 分钟 | 重新连接页面/重新登录 | 仅影响浏览器会话，与上述凭据无关 |

**关键机制：**
- 刷新是**轮换式**：旧 refresh token 换新后立即作废，Relay 保留到原窗口结束用于检测重用。**若用旧 token（例如从备份恢复的 Keychain/凭据文件）再次刷新，Relay 会判定重放并撤销整个身份**（`store.go:527`，错误码 `refresh_reuse_identity_revoked`）。因此任何情况下都不要恢复旧 refresh token、不要复制凭据文件。
- 刷新采用两阶段写入（先写 `refresh_pending` 标记再请求），崩溃后状态显示为 `credential_recovery_required`，恢复见第 3 节。

## 2. 日常更新：正常情况下不需要任何手动操作

Hub 常驻 WSS 连接在线时：
- 到期前 1 分钟自动刷新 access token 并滚动 30 天 refresh 窗口（第九批"活动连接续期"）；
- 断线后以 1s→30s 指数退避重连，重连本身也会触发临期刷新；
- APNs 推送登记、模块名发布共享同一凭据链。

**手动核对/强制刷新（排障用）：**

```sh
# 只读核对 + 验证身份与 WSS 握手（会顺带刷新 access token）
djonehub remote status --profile <配置名>
djonehub remote check   --profile <配置名>
```

网页端：打开 `http://127.0.0.1:7575/remote/` → 对应配置卡片 → "刷新"。`access_expires_at` 向前滚动即代表刷新成功。

## 3. 故障恢复

| 现象 | 含义 | 处理 |
| --- | --- | --- |
| 页面报 `keychain_unavailable` | **Hub 进程读不到 macOS 钥匙串**（最常见：从 SSH/后台守护终端启动，非 GUI 会话） | 见第 4 节，从 GUI 会话重启服务 |
| 页面报 `credential_recovery_required` | 上次刷新事务结果不确定（`refresh_pending`） | 先"刷新"核对结果；不要重新导入旧授权。若仍不能恢复，用新申请重新登记（第 5 节），旧身份由 Relay 管理员撤销 |
| `remote status` 报 401 `invalid_credential` | refresh 窗口已过 30 天 | 无法自助恢复，按第 5 节重新登记 |
| 401 `refresh_reuse_identity_revoked` | 检测到旧 token 重放，身份已被自动撤销 | 重新登记；并排查是谁在用旧凭据（备份恢复、双进程等） |
| iPhone 端"凭据更新未完成" | 手机端刷新事务中断 | 按 App 恢复页提示处理；不要重复扫同一二维码 |

## 4. 本机根因专节：Keychain 会话问题（2026-11 排查实录）

**症状**：`/remote/` 页显示连接 `disabled`，手动连接任务 12ms 内失败，错误码 `keychain_unavailable`；`djonehub remote check`（在别的会话里）却正常。

**根因**：Hub 由 `herdr`（launchd 直接派生的后台守护进程）的终端启动，处于**非 GUI 安全会话**。钥匙串读写代码（`internal/relayprofile/keychain_darwin.m:12-21`）设置 `LAContext.interactionNotAllowed = YES`——需要任何用户交互（ACL 确认弹窗）的钥匙串操作会**立即失败**而不是等待。GUI 会话（Terminal.app、或 GUI 应用派生的终端）则一切正常。

**结论与规则**：
- **必须从 GUI 登录会话启动 DJOneHub**：Terminal.app / iTerm 直接运行 `djonehub start --remote-profile <配置名>`；
- SSH 会话、`herdr`/tmux 等后台守护终端启动的实例无法刷新凭据，远程接听会静默失效；
- 升级二进制后首次启动若弹出"允许访问钥匙串"提示，点"始终允许"。

**推荐长期方案：LaunchAgent（GUI 域守护，开机自启 + 崩溃重启）。** 保存为 `~/Library/LaunchAgents/work.oops.djonehub.plist`：

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>work.oops.djonehub</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/libexec/djonehub/bin/djonehub-macos</string>
    <string>--listen</string><string>127.0.0.1:7575</string>
    <string>--remote-online-profile</string><string>web_71cb199b0eac7c2d7a936dea1f7c99c7</string>
    <string>--native-control-file</string>
    <string>/Users/yuyi/Library/Application Support/DJOneHub/native-control/bootstrap.json</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>/Users/yuyi/Library/Logs/DJOneHub/djonehub.log</string>
  <key>StandardErrorPath</key><string>/Users/yuyi/Library/Logs/DJOneHub/djonehub.log</string>
</dict>
</plist>
```

启用：`launchctl load ~/Library/LaunchAgents/work.oops.djonehub.plist`（需先 `djonehub stop`）。注意：LaunchAgent 方式不经过启动器脚本，`djonehub stop/status` 不再管理它；AudioHost 需按需另行处理或保持现有启动器方式。首次 load 后在台前操作一次，允许钥匙串访问提示（如出现）。

## 5. 重新登记流程（仅在第 3 节判定无法恢复时）

1. Relay 服务器上执行 `djonehub-relay-admin` 的撤销（或让管理员在管理页撤销旧 Hub 身份），连带清理其下手机安装；
2. Mac 端 `djonehub remote enroll --profile <新配置名> --relay https://oops.work --enrollment-file <新签发的grant.json>`（离线文件 10 分钟一次性）；
3. 网页 `/remote/` 连接新配置；原有配对手机需重新配对（旧安装身份随旧 Hub 一并失效）。

已有配对不需要因升级二进制而重建——升级既不清 Keychain 也不动 Relay 数据。

## 6. 长期有效性评估结论

**问题：能否让某台 Mac 配对（登记）后长期有效？**

**结论：体系设计上已经支持"一次登记、无限期有效"，无需改造。** 30 天滑动窗口 + 常驻自动续期意味着只要满足以下三个前提，凭据永不失效：

1. **Hub 运行在能访问钥匙串的会话**（第 4 节；这是 2026-11 本次故障的真正根因，已通过 GUI 会话重启解决）；
2. **30 天内至少成功连接一次**（正常常驻运行每天都连接，余量极大；即使 Mac 休眠两周也没问题）；
3. **绝不恢复/复制旧凭据**（重放检测会立即撤销身份，这是刻意的安全设计）。

iPhone 端同理：安装身份无固定寿命，30 天内打开一次 App 即可维持；长期不用的手机过期后重新扫码配对即可。

**如确实需要更长窗口**（例如 Mac 会长期离机存放）：修改 Relay 端常量 `RefreshLifetime`（`internal/relay/identity/store.go:35`，当前 `30 * 24 * time.Hour`）并重启 Relay。**不建议**：窗口越长，泄漏的 refresh token 可用时间越长；滑动窗口 + 高频续期的现有设计在安全与便利间是合理的平衡点。

## 7. 相关文件位置

| 内容 | 路径 |
| --- | --- |
| Hub 凭据（Keychain） | 服务 `org.djonehub.remote.identity.v1`，账户为 profile 名 |
| Web 管理（回环）状态 | `~/Library/Application Support/DJOneHub/remote-web/` |
| Web 管理（局域网）状态 | `~/Library/Application Support/DJOneHub/web-lan/`（首次建号的 setup.json 于此） |
| Web 访问配置 | `~/Library/Application Support/DJOneHub/web-access.json` |
| Relay 身份数据库 | Relay 服务器 `--state-dir` 下的 SQLite |
