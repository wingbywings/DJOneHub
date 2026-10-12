# 第十七批：R07 系统音频打断恢复

日期：2026-10-11。分支：`codex/remote-iphone-callkit`。工作区实现，未提交；本批没有更新运行中的 Hub、AudioHost、iPhone 或公网 Relay。

## 已实现的范围

新增默认关闭的 `--remote-audio-recovery`，同时启用信令/ICE 恢复。在初始 offer 的 `audio_recovery` 字段协商系统音频短暂打断能力，旧行为仍立即结束通话。只允许已经接通的通话暂停；振铃/首次音频启动不借此延长接听预算。

CallKit didDeactivate 后，手机立即停用本地媒体，发送带单调 sequence 的 audio_active=false；保留原 CallKit 通话、UUID、媒体 generation 和 PeerConnection。Hub 同步停止双向音频，清空 Sender、Receiver 及 AudioHost 桥接队列，保留蜂窝通话和宿主租约。暂停不是静音按钮，不改变用户静音设置。

第一次打断建立固定十秒期限。重复 false、较旧 sequence、状态查询或 WSS 恢复都不延期。手机重新收到 didActivate 时，发送新的 audio_active=true；Hub 核对蜂窝、模块、宿主、传输状态及新的音频 sequence 后恢复媒体。恢复不重复 ATA，也不再次完成 CallKit 接听动作。即使 false/true 在同一工作轮询间隔内连续到达，暂停标记也不会丢失。

WSS 恢复完成后，手机重发本通话最新的 audio_active 状态，仍使用原 sequence，避免丢失打断或恢复通知。DataChannel 不因此获得音频激活权限。手机在打断期间收到 Hub connected 状态时保留暂停和超时，不错误开启音频。

超时、通话结束、模块失效、挂断和宿主终止仍按原有收尾执行；迟到 didActivate 不能复活结束的通话。新来电重置前一通的暂停期限。暂停和恢复清缓存不重置 RTP/PCM 的 generation、SSRC、防重放历史及宿主 token。

## 验证

- Go 会话竞态测试覆盖正常/立即恢复、重复打断不延期、旧 sequence、固定超时、挂断、蜂窝失活、模块失效和默认旧行为；ATA 和最终清理各一次。
- 真实 Opus 回归覆盖直连、trickle、ICE restart，以及强制 TURN/UDP 和 TURN/TCP；暂停期间编码/解码计数不增长且缓存为空，新可信音频状态到达后双向解码继续，宿主绑定不变。
- Swift PhoneCore 覆盖 CallKit 门控、重复打断、暂停期间状态核对、超时激活、挂断后激活和下一通隔离。
- iOS Xcode build-for-testing 构建成功。未用模拟器冒充真实系统电话打断，也未通过测试采集麦克风声音。

本机日志：`/tmp/djonehub-r07-media.log`、`/tmp/djonehub-r07-swift.log`、`/tmp/djonehub-r07-ios-build.log`、`/tmp/djonehub-r07-full-regression.log`。

## R07 尚未关闭的部分

AudioHost 进程/PCM 连接短暂断开仍会终止本通。现有 HostBridge 的一次性 token、防重放序号和 backend 租约绑定禁止直接重复使用已断开的 socket。宿主恢复需要通过可信 native-control 重新核对同一蜂窝通话、签发新宿主连接凭据、处理新旧音频队列和宿主计数，尚未实现；本批不能标为完整 R07。

真实 iPhone 的系统电话打断、AirPods/听筒/外放路由切换、连续打断及与 Wi-Fi/蜂窝切换重叠的验收仍待三端升级后执行。十秒内没有再次收到系统音频激活就结束本通，不自动抢占系统电话音频。

后续继续 R07 宿主恢复，以及 R08 DTMF/Hub 静音、R09 通话 journal、R10 宿主事件订阅、R11 配对管理/扫码/手机历史；R06 的新旧通路并行迁移与公网到期验收仍保留未完成。
