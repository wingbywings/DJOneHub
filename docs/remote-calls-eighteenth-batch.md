# 第十八批：R08 远程拨号键与 Hub 静音同步

日期：2026-10-12。分支 `codex/remote-iphone-callkit`，未提交。没有升级运行中的 Hub、Relay 或 iPhone。

## 实现

- 新增默认关闭的 `--remote-call-controls`。Hub offer 的 `call_controls` 能力字段控制 iOS 是否开放 CallKit DTMF 和 App 的 12 键键盘；启用前需升级 Relay/Hub/iOS，旧严格解析器不能接受新字段。
- DTMF 仅接受已接通、音频已激活通话的单个 `0–9 * #`。核对安装身份、UUID、generation、revision，沿既有受绑定的硬件命令路径再次查询 CLCC 后发送 VTS；模块明确拒绝时沿既有 CLDTMF 兼容路径处理。
- 同一 request_id 的重试共用结果，包括超时/失败；不同数字复用 request_id 被拒绝。每通只允许一个正在执行的按键，最小间隔 120 ms，硬件操作最多 2 秒。iOS 等待确认最多 4 秒，失败只结束该按键动作，不结束整通电话；不自动重发结果不确定的数字。挂断可取消正在执行的硬件按键。
- CallKit 静音立即关闭手机本地音轨，同时发送独立单调 sequence 的 `mute` 状态。Hub 同步丢弃上行 RTP、清空解码/待播放队列；下行继续。取消静音不会重新激活已暂停的音频。
- 长静音期间继续核验 SSRC/generation/重放窗口并推进序号，不保存或解码声音，避免超过 512 包或序号回绕后无法恢复。WSS 续通重发最新静音状态；重复/旧 sequence 不能反转更新状态。
- DataChannel 仍只允许预绑定通话的 sync/hangup；不能绕过 WSS 发 DTMF 或静音控制。Hub 媒体状态增加 `uplink_muted`。

## 验证

- 会话竞态测试：幂等、冲突 request_id、身份/generation 隔离、无效字符、未接通/未协商控制、并行按键、挂断取消、未知结果不重试、静音顺序与终态隔离。
- 真实鉴权 Relay/WSS 测试：能力协商、DTMF/静音确认、重复发送硬件仅执行一次、旧静音不能覆盖新状态、挂断清理。
- 实际 Opus/直连/强制 TURN 回归：Hub 静音时上行解码停止而下行继续，取消静音恢复双向音频；音频暂停期间切静音不能开启媒体。
- Receiver 加速测试覆盖 70,000 个静音包（超过 23 分钟和一次序号回绕），不保留/播放静音期间声音，恢复后仅播放新包。
- Swift PhoneCore 共 28 项通过；iOS build-for-testing 通过。Go 全仓库 race 回归通过（与第十九批一起执行）。

日志：`/tmp/djonehub-r08-controls-tests.log`、`/tmp/djonehub-r08-relay-controls.log`、`/tmp/djonehub-r08-swift.log`、`/tmp/djonehub-r08-ios-build.log`。

## 验收边界

尚未升级真机；运营商/模块对 VTS、CLDTMF 的实际 IVR 识别，以及锁屏键盘、长静音后恢复需真机验证。清空的是 Hub 内部及尚未发出的 PCM 队列；已经交给系统/设备的短播放缓冲不能撤回，不承诺采样级瞬时静音。DTMF 不支持长按 start/stop、多数字串或 A–D。
