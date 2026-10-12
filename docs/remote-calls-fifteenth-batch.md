# 第十五批：ICE 恢复与信令续通基础

日期：2026-10-11。分支：`codex/remote-iphone-callkit`。本记录对应未提交的工作区变更；本批没有更新正在运行的 Hub、AudioHost、iPhone 或公网 Relay，也没有生成新版发行包。

> 后续状态：R05 产品自动重连、状态核对、备用 DataChannel 和 R06 有界续租已继续接入，见[第十六批](remote-calls-sixteenth-batch.md)。以下“尚未接入”描述保留为本批历史状态。

## R04：已实现的部分

Hub 增加默认关闭的 `--remote-ice-restart`。启用后初始 offer 显式声明恢复能力；连接曾经成功建立、会话已接通，且蜂窝、模块、CallKit 音频条件仍有效时，短暂 ICE 断开不立即释放蜂窝通话和 AudioHost 租约。断开持续 500 ms 才尝试恢复，每次故障最多一次，固定 10 秒预算；超时仍执行正常收尾。

恢复由 Hub 发起，保持同一通话 UUID、媒体 generation、PeerConnection 和原生宿主绑定，不重复 ATA 或再次激活音频。`restart_offer` / `restart_answer` 携带递增 `negotiation_id` 和收集完成的 SDP；重协商后不再接收初始 trickle ICE。校验 SDP origin 会话 ID、递增版本、变化的 ICE ufrag/password 和原有通话绑定，拒绝交叉会话、跳号、旧 SDP、旧 answer、旧候选及重叠协商。

iOS 使用既有 PhoneMedia 回答恢复 offer，保留 CallKit 的音频激活门控。disconnected/failed 在协商了此能力时允许有界恢复；默认行为保持原来的失败收尾。

同时修正长网络中断后的 RTP 接收窗口：平常保留 512 包的前向界限；只有有效 ICE 重协商才打开一次有时限的扩大窗口。恢复包仍受 generation、SSRC、已解码时间戳防重放、时间跨度和序号上限约束，不能借此替换音频流或重放已播放的语音。

### 验证证据

- Go/Pion 两次连续 ICE restart 后，双向实际 Opus RTP 继续解码，AudioHost 绑定和激活状态保持不变。
- 真实 TLS Relay + 会话状态机测试：恢复后 ATA 与媒体激活各执行一次；取消后正常结束且无清理错误。
- 原生 iOS 模拟器 / WebRTC 154 与 Pion 互通：两次 restart 后 RTP 接收计数继续增长，CallKit 激活前音频仍关闭。Xcode `build-for-testing` 和 `test-without-building` 成功，测试未跳过。
- Swift PhoneCore 24 项测试通过，包括协商顺序、旧 SDP 与错误绑定。
- Receiver 的 15 秒输入中断仿真恢复解码；无协商、过期、错误 SSRC/generation、未来时间戳、过大序号、重复包和已解码语音均被拒绝。
- `go test -race -tags remoteopus` 的媒体、会话、协议、Relay、Hub 回归通过；新增 R05 后全仓库 `go test -race -tags remoteopus ./...` 最终通过。

这里验证的是重协商和有界恢复机制，**不是实际 Wi-Fi/蜂窝切换验收**。WSS 一并中断时，当前产品调用链仍会结束通话；完整 R04 关闭依赖 R05 接入及真实切网测试。旧 Relay 不理解新的 SDP 字段/消息，因此本开关只可在三端同步升级后试用。

## R05：已交付的协议基础，尚未接入产品自动恢复

Relay 与 Go 信令客户端增加显式协商能力：

- `X-DJOneHub-Recovery: 1` 请求支持续通，welcome 按需返回 `recovery`。默认请求和 welcome 不新增字段，保持已有客户端行为。
- 仅双方都声明能力、且 Hub 已发布 connected 的路由允许短断续通。振铃、已结束、已丢失的 Relay 进程内路由不能恢复。
- `X-DJOneHub-Resume` 是原通话 envelope 的无填充 base64url 编码（type 为 resume，payload 为空）；它不是访问凭据，仍必须通过 Bearer 鉴权。握手核对原通话所有绑定字段、身份、角色、tenant、Hub、身份 version 及完整模块范围。
- 保留原通话 epoch 和 UUID 防重放记录；认证成功的恢复可替换同身份的半开旧 socket，旧连接取消且不能再路由命令。welcome 的 `resumed` 用于确认此次握手确实恢复了既有路由。
- 首次故障建立固定 10 秒期限；双方断开、失败升级、重试均不延长期限。`peer_reconnecting` / `peer_resumed` / 超时 `peer_closed` 只描述路由连接状态。
- 暂停期间拒绝控制与 SDP 投递，不缓存或重放命令；Hub 的 ended 状态仍立即删除路由。恢复后必须由调用方重新核对状态。
- 新 Go `ClientConfig.Resumption` / `Resume` 只提供显式握手能力，不会自行重连。现有 Hub/iOS 没有打开它，因此不能把这些基础接口当成已上线的自动续通功能。

测试覆盖 Hub 或手机断开重连、双方同时断开、半开连接替换、旧定时器不终止已恢复通话、暂停期间不缓存消息，以及错误身份/绑定、缺少能力、振铃、已结束、超时和撤销后的拒绝。

## 接下来按顺序接入

1. R05 产品调用链：Hub 连接管理器移交恢复生命周期；iOS 保留 PhoneCall/CallKit/媒体实例；固定预算内重新鉴权连接，核对最新 revision/终态；处理协商应答丢失时的幂等重发。
2. R05 备用 DataChannel：只接受既有授权通话的状态核对、挂断及终态；与 WSS 共用绑定和命令幂等处理。完成后再统一启用 R04/R05，生成用户自行部署的 Relay 升级包及配套客户端。
3. R06：基于已验证的 ICE restart 刷新 TURN 配置，在到期前切换并验证 allocation；失败仍保留明确截止时间。
4. R07：区分系统音频短暂打断和终止；宿主恢复使用有期限的租约、清空旧 PCM 并重新核对路由。
5. R08：远程 DTMF、Hub 侧静音同步；严格限制 AT 参数，静音清队列，保持顺序和幂等。
6. R09：原子写入最小通话 journal，启动时核对 CLCC、释放孤儿租约，不自动重拨、重接或重推。
7. R10：在已有 CLCC 轮询优化基础上，增加 AudioHost 事件订阅及断线后的状态全量核对。
8. R11：Mac 配对/远程管理入口、iPhone 扫码与本机通话历史；沿用已有模块别名和 Mac 历史。

以上后续项目没有因本批验证而自动完成；真实切网、TURN 过期、系统音频中断和应用/服务重启仍需要对应实现后的验收。

## 最终验证记录

全仓库竞态测试首次发现两处原有测试同步缺陷：`apduarbiter` 的优先级老化测试退出时没有等待所有获取任务，`esim` 的延迟刷新测试无锁读取缓存并依赖相对睡眠时序。本批仅修正这两个测试的等待、缓存读取及刷新时刻断言；没有修改这两项业务实现。受影响包复测通过，随后全仓库竞态回归通过。

原生互通测试输出：`/tmp/djonehub-r04-native-ios.log`、`/tmp/djonehub-r04-native-go.log`；全仓库最终输出：`/tmp/djonehub-r04-r05-regression-final.log`。这些是本机临时验证日志，不包含实际电话音频，也不属于发行产物。
