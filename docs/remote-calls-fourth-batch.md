# 远程接听：第四批 WebRTC 与接收音频

日期：2026-10-01。分支：`codex/remote-iphone-callkit`。

后续控制层进度见 [第五批会话协调与通话收尾](remote-calls-fifth-batch.md)。以下保留第四批交付范围。

本批打通了两个本机 WebRTC 端点之间的双向 Opus 音频，并加入接收抖动缓冲和媒体启动条件。**这些组件尚未接入实际远程来电流程，iPhone 仍不能接听。** 远程状态继续返回 `available: false`；AI 分支不创建远程 PeerConnection，也不发送远程邀请。

## WebRTC 传输

新增 `internal/remotemedia.Peer`，固定使用 Pion WebRTC v4.2.22。仅协商 Opus，RTP 时钟为 48 kHz，实际模块 PCM 为 8 kHz 单声道；SDP 声明单声道、FEC 和码率上限。发送端延续 20 ms 打包、默认 16 kbps、12–24 kbps 可配置范围，初始化随机 RTP 序号和时间戳。

- Pion 处理 ICE、DTLS、SRTP 与 RTCP reports；本批不启用音频 NACK/重传。
- 提供 offer/answer 和 ICE candidate 接口，全部绑定 call UUID 与 media generation；拒绝错配、过大 SDP/candidate 和候选积压。
- 支持传入 ICE servers 和 relay-only 配置。默认使用 UDP4/UDP6；TCP/TLS TURN、公网 NAT 与网络切换尚未验证。
- 当前 offer/answer 接口等待本地 ICE gathering 完成，同时提供 candidate 事件。调用方必须传入有截止时间的 context；还没有接入公网信令协议或优化完整的 trickle 流程。
- Peer 拥有独立的 Opus codec、发送队列、接收缓冲和本机 HostBridge。宿主断开、传输失败、父 context 取消时终止媒体并回收资源；上层后续仍需把失败转为本通蜂窝电话的结束动作。

实现依据：[Pion WebRTC](https://github.com/pion/webrtc)、[v4 API](https://pkg.go.dev/github.com/pion/webrtc/v4)。

## 接收缓冲与丢包

新增 `Receiver`，锁定单个 SSRC 和 generation，展开 16 bit 序号与 32 bit RTP 时间戳，处理回绕、乱序、重复及重叠包。接收窗口限制为前方 200 ms、最多 64 包；Opus 负载最多 1275 字节，支持 2.5–120 ms 包长。

首次播放与重新蓄水采用固定 60 ms 延迟。每个实际 10 ms tick 最多输出一帧 PCM，不批量追赶播放。编码与播放按独立样本时钟推进；发送侧丢帧保留 RTP 时间轴上的跳跃。

- 确认恰好缺少一个 20 ms 包且后继包已到达时，先请求 FEC，再正常解码后继包。
- 连续丢包最多补偿 60 ms，之后重置解码器并输出静音。FEC 请求不代表负载确实携带冗余；没有冗余时 libopus 可能使用 PLC。
- 调度停顿超过播放截止时间 40 ms 时丢弃已解码积压，仅保留最近到达且未超过 60 ms 的最新包，重新蓄水。
- 暂停清除发送和接收缓冲，同时保留 SSRC、重放检查及输出帧身份，避免恢复后复用旧音频身份。

当前仍是固定缓冲；自适应延迟、长期采样时钟漂移补偿、RTCP 驱动的码率/FEC 调整尚未实现。不能据此宣称弱网或长通话音质达标。

## 接听与音频激活分离

Peer 接受由可信 Coordinator 构造的 `Readiness` 快照，包含 call UUID、generation、单调 revision、蜂窝 active、模块 ready、iOS audio active 和 ended。revision 独立于通话命令 revision；相同快照重试幂等，旧版本或同版本冲突拒绝，终态不能复活。

`AnswerReady()` 要求蜂窝已接通、模块就绪、ICE/DTLS 已连接、本机宿主已连接。它不等待 iOS audio active，以允许后续 iOS fulfill CallKit 接听动作。

`Activate()` 额外要求 iOS 已确认音频激活。连接建立、SDP 交换或收到音频包都不能单独启动媒体；就绪条件撤销后停止编码及接收缓冲，并要求再次显式激活。该门禁只控制本 Peer 的音频泵；已写入宿主/硬件的短缓冲仍需由后续宿主控制协议清理。

**Readiness 不是网络请求 DTO，也没有实现身份认证。** 后续 Coordinator 必须从 Hub 的实际通话/UAC 状态及已授权安装实例的消息生成快照，不能直接相信客户端上报蜂窝或模块就绪。本批未把 HostBridge 挂到 app HTTP 路由，也未向普通宿主配置或浏览器接口发布媒体凭证。

## 验证与依赖

自动化集成测试创建两个真实 Pion PeerConnection，使用本机 UDP 完成 ICE/DTLS 握手，通过 SRTP 双向传送真实 libopus 编解码后的音频，再经两个经过 token 认证的 HostBridge 输出 PCM。测试覆盖激活前禁止转发、answer-ready 不等待 iOS 激活、撤销就绪后停止媒体、过期状态拒绝，以及宿主断线取消。

接收器测试覆盖乱序、序号/时间戳回绕、重叠包、FEC 调用顺序、有限 PLC、静音、120 ms 包、2.5 ms 包拼接、调度停顿及重置后的重放拒绝。

```sh
go test ./...
go test -race -tags remoteopus ./internal/remotemedia/... ./internal/callcontrol ./cmd/djonehub-macos -timeout 90s
```

上述验证需要真实 libopus 开发库，以及允许测试创建 loopback TCP/UDP socket。没有 `remoteopus` 或关闭 cgo 时，创建 Peer 明确返回 codec unavailable。

本批已通过全量 Go 测试、上述真实 Opus/WebRTC 竞态测试和 `git diff --check`。

新增依赖已固定在 `go.mod` / `go.sum`。仓库现有本地替换目录裁剪了 `x/sys/windows` 与部分 `x/text` 包，导致跨平台/依赖测试解析阶段的 `go mod tidy` 无法完成；本批保留这些替换并整理直接依赖声明，以实际 macOS 构建和测试验证。该依赖整理限制仍需后续修复。

## 后续工作

1. Coordinator 接入授权 remote owner、answer deadline、媒体失败收尾，及受认证的本机凭证分发/UAC 控制。
2. 配对与安装身份、公网 WSS 信令、APNs VoIP Push、TURN 凭证与部署。
3. 原生 iOS CallKit/PushKit/libwebrtc App、音频会话激活和网络切换。
4. 自适应抖动缓冲、漂移补偿、RTCP 反馈、ICE restart、静音，以及正式 Opus 打包。
5. 实际 iPhone、模块、CoreAudio、后台唤醒和公网弱网联调；现有本机互通测试不能替代这些验收。
