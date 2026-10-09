# 远程接听：第五批会话协调与通话收尾

日期：2026-10-01。分支：`codex/remote-iphone-callkit`。

后续宿主控制进度见 [第六批受认证的本机宿主控制](remote-calls-sixth-batch.md)。以下保留第五批交付范围。

本批新增远程会话协调器，并通过内部适配器连接已有通话命令、跨模块音频租约及第四批媒体接口。**尚未连接公网信令或受认证的宿主控制通道，也没有 iPhone App；现有来电流程不会自动创建该会话，远程状态仍为不可用。** 以下是可测试的内部实现，不是已开放的公网 API。

## 会话状态与时间预算

`internal/remotecall.Session` 绑定 `{call_uuid, installation_id, generation}`，构造时先检查不可变远程路由和目标安装实例，再调用媒体工厂。AI/本地路由直接拒绝，不创建 PeerConnection。

```text
offered → answering → awaiting_audio → connected → ended
   └──→ rejecting ───────────────────────────────→ ended
```

- `answer` 只确认接听意图；取得租约后等待 WebRTC transport connected，再通过既有接听命令准备模块并发送 ATA。
- `awaiting_audio` 对应 `answer.ready`：蜂窝已 active、模块路由与可信宿主均就绪、ICE/DTLS 和 HostBridge 已连接。此时 iOS 才应 fulfill CallKit 接听动作。
- 收到正确安装实例、通话和 generation 的音频激活确认后才调用媒体 `Activate()`，进入 `connected`。
- `reject` 从 offered 发起，不等待 ICE；通过绑定原通话的拒接命令执行。
- `hangup` 用于接听准备或接通之后，取消任务并按 ATA 是否可能已发送决定是否需要蜂窝挂断。振铃阶段的主动拒接使用 `reject`。
- 邀请过期只撤销邀请，不挂断仍在振铃的蜂窝电话。本地先接听时，邀请以 `answered_elsewhere` 结束。

振铃截止时间来自原通话开始时间和路由快照；用户点击后的接听准备默认 12 秒，answer-ready 后等待 iOS 音频激活默认 3 秒，独立收尾预算默认 12 秒。媒体断开会取消正在执行的接听准备。`Done()` 在媒体关闭和收尾尝试完成后关闭，不能把会话 ended 当作蜂窝已确认结束。

当前 iOS 音频停用、已连接阶段的模块/传输失效会结束会话；暂未实现中断恢复及 10 秒 ICE restart 恢复窗口。状态通过快照读取，公网事件订阅与 CallKit 来电展示确认仍待接入。

## 命令与身份边界

会话命令带 request ID、expected revision、call UUID 和 media generation；重复请求先查去重结果，再校验状态版本。同一 ID 改写内容会冲突。音频激活使用独立单调序号，旧激活不能覆盖较新的停用或复活终态。

`installationID` 必须由将来的已认证连接提供。它目前只是内部绑定校验，不能替代配对、凭证验证或撤销检查。构造器和会话方法没有注册为 HTTP 接口。

Mac 适配器的 remote owner 是进程内对象，通过私有 context key 进入既有通话命令。普通 JSON 不能创建该对象，也不能通过设置 actor 获得远程控制权。执行时再次验证当前 owner、路由和目标；现有本地 HTTP 入口仍强制使用 local actor。

每模块同时只保留一个未清理完的远程会话。同一通话重建时要求更大的 media generation。会话与原通话 context 关联，来电替换或结束会取消旧任务。

## 资源和不确定结果

- 远程 claim 在同一个通话锁内检查 incoming、控制状态、路由和目标，并申请共享 AudioArbiter。成功后记录 `control_owner: remote`。本地接听与远程 claim 只有一个赢家。
- ATA 之前失败可释放远程 owner 和音频租约，保留本地接听机会。
- AT 传输调用前已取消的请求明确记录为未尝试发送，不因取消而误走结果未知的挂断流程。
- 通话命令新增内部 `MayHaveAnswered` 结果：已尝试发送 ATA，即使超时或缺少 OK，也需要核对原通话后收尾；模块明确 ERROR 则不按已接听处理。该字段不出现在 JSON 响应中。
- 收尾使用新的有限 context，通过既有 UUID/CLCC 校验发送挂断，不重发 ATA。旧回调不能改为操作当前新通话。
- 活动、answer-pending 或 ending-pending 通话保留租约直到既有 CLCC 生命周期确认结束。挂断结果不明或 CLCC 查询失败时保留 owner，并在会话快照中记录 `cleanup_error`，不虚报资源已清空。
- 收尾只做一次受控尝试；自动重试或异常电话人工处置的界面仍待实现。原有 AT 协议不携带 UUID 的硬件竞态限制仍然存在。

## 本机音频就绪边界

Coordinator 读取 Hub 的蜂窝/模块事实，额外要求绑定当前会话的可信宿主就绪证明；未提供该证明时始终不满足 ModuleReady。旧公开 `audio/host/register` 的 running 字段不会授权远程媒体启动。

远程 owner 存在时，普通宿主配置明确返回 remote 模式并关闭录音。该接口不发布远程媒体 token；Swift 宿主缺少远程凭证时停止启动，不回退到 Mac 麦克风/扬声器模式。

受认证的宿主启动、凭证分发、就绪确认和缓冲清理协议仍待实现。因此当前内部会话适配器不能直接启用在真实来电中。

## 验证

新增 19 个协调器与 Mac 适配器测试函数，覆盖：

- AI 路由不创建媒体、错误安装实例/generation、重复命令与过期版本。
- ICE 前不发送 ATA、answer-ready 前拒绝音频激活、激活前不启动媒体。
- 传输、模块和音频激活超时，不同阶段对应的资源释放/挂断。
- 媒体断开取消在途 ATA，结果不明时执行绑定原通话的清理。
- 振铃超时、本地获胜、远程拒接、正常接听/激活/挂断完整状态链。
- 本地与远程并发抢接只有一个赢家；旧清理不能挂断新来电或释放新租约。
- 未确认蜂窝结束时保留占用、清理错误可观察、旧宿主上报不能伪造就绪。

Mac 会话测试使用模拟媒体和模拟蜂窝设备，验证控制逻辑；第四批真实 Pion/Opus loopback 测试继续验证媒体链路。两者均不能替代 iPhone、模块及公网端到端验收。

```sh
go test ./...
go test -race -tags remoteopus ./internal/remotecall ./internal/remotemedia/... ./internal/callcontrol ./cmd/djonehub-macos -timeout 90s
```

本批已通过全量 Go 测试、上述真实 WebRTC/Opus 竞态测试及 `git diff --check`。最后的 AT 取消边界修正另通过协调器、通话控制及 Mac app 的竞态回归。

## 下一步

1. 受认证的本机宿主控制入口与生命周期消息，分发 per-call 媒体凭证并验证实际 UAC ready。
2. 配对/凭证存储与撤销、安装身份、公网信令接入和事件同步，把内部会话连接到真实来电。
3. APNs/TURN、iOS CallKit/PushKit/libwebrtc App 及真机测试。
4. ICE restart、网络恢复、DTMF/静音的远程会话命令，以及第四批列出的自适应缓冲、时钟补偿和发布工作。
