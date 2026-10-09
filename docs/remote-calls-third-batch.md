# 远程接听：第三批媒体基础

日期：2026-10-01。分支：`codex/remote-iphone-callkit`。

后续进度见 [第四批 WebRTC 与接收音频](remote-calls-fourth-batch.md)。以下保留第三批交付时的范围。

本批实现 P2 的独立本机媒体通道、短队列与真实 Opus 编解码基础。**尚未完成 WebRTC 传输与 iPhone 通话，也未完成整个 P2。** 远程状态接口继续返回 `available: false`，不会因编译了 Opus 就发送远程邀请。

## 已实现

### 独立 Swift 远程模式

- 音频宿主识别 `manual / agent / remote`。远程模式独占模块 UAC，不请求或启动 Mac 麦克风、扬声器，也不把 PCM 送到 AI 的媒体客户端、整段回复队列或录音器。
- 新增 `RemoteMediaClient`，只连接字面 loopback 地址，使用 Authorization header，验证 ready 消息中的 call UUID、media generation、协议版本及音频格式。
- 单 writer、最多 6 个待发送 PCM 帧、100 ms 写等待期限；过期帧丢弃，旧连接回包受连接 epoch 检查。失败后停止媒体，不在同一连接上补发历史音频。
- Swift 播放队列上限 60 ms，并按等待年龄剔除超过 60 ms 的 PCM；每次向 UAC 供数最多 10 ms，未被 UAC 接受的样本继续保留在短队列内。
- C 层新增远程专用读写接口：采集积压超过 60 ms 时由消费者丢弃旧样本，并向上层报告跳过的样本数；播放环形缓冲最多保留 20 ms。原 AI/本地接口不使用这些新限额。
- 为避免短缓冲与过大的硬件回调周期冲突，远程启动要求 CoreAudio 报告的双向缓冲周期均为 1–160 样本（8 kHz 下不超过 20 ms）。不符合时明确失败；尚未自动修改设备 buffer frame size，需在 P0 真机验证时确定协商与恢复方式。
- USB 启动后的远程配置再次核对 UUID / generation；异步启动受宿主 lifecycle generation 和取消检查约束。remote 模式还强制忽略录音开启请求。

### Go 本机媒体桥

`internal/remotemedia.HostBridge` 是独立、单次使用的 HTTP/WebSocket handler：

- 只接受真实 TCP peer 为 loopback 的请求，不相信 X-Forwarded-For；拒绝带 Origin 的浏览器请求。
- 每个 bridge 生成 256 bit 随机宿主 token，绑定 call UUID 和 media generation；仅允许一个连接。桥关闭后，重连必须由上层创建新 generation/token。
- 上下行队列各最多 6 帧、60 ms；输出每个实际 10 ms tick 最多写一帧，不追赶时钟批量补发。
- 验证帧长、方向、generation、sequence 和 sample index，拒绝重放与重叠；3 秒无输入/health、写超时或协议错误终止该 bridge。

**此 handler 尚未注册到 app 路由。** 现有 `/audio/host/config` 也没有返回 remote 凭证。后续必须先在 Coordinator 中接入远程 owner、answer-ready、ICE 及音频激活状态，再通过受限宿主控制入口发布凭证和挂载 handler；普通状态/浏览器接口不得泄漏 token。Swift 已支持届时的 `call_uuid`、`media_generation`、`remote_media_url`、`remote_media_token` 可选字段。

### PCM 协议 v1

固定 s16le / mono / 8 kHz，每条本机二进制消息恰好 192 字节，包含 80 样本（10 ms）音频。头部整数均为 little endian：

| 偏移 | 长度 | 字段 |
| --- | --- | --- |
| 0 | 4 | ASCII `DJRM` |
| 4 | 1 | 版本 1 |
| 5 | 1 | 方向：1=模块到网络，2=网络到模块 |
| 6 | 2 | 样本数，固定 80 |
| 8 | 8 | media generation，非 0 |
| 16 | 8 | 单调递增 sequence |
| 24 | 8 | 本方向累计 sample index |
| 32 | 160 | PCM 样本 |

捕获分帧器允许 UAC 提供不同大小的 PCM 块，并保留不足一帧的尾部。丢弃捕获音频时 sample index 跳过对应样本，不压缩时间。Go 与 Swift 共用 `internal/remotemedia/testdata/pcm-v1.hex` 作为协议样例。

### 真实 Opus 与发送时钟

- 新增 `internal/remotemedia/opus`，通过 cgo 使用本机 libopus；开发构建检查版本为 1.5.2。
- 编码使用 8 kHz 单声道、160 样本/20 ms、16 kbps 默认码率、constrained VBR、complexity 5、语音模式、最大窄带、FEC 开启、初始丢包估计 5%、DTX 关闭。提供 12–24 kbps 与丢包估计调整入口，尚未接 RTCP 反馈。
- 支持正常解码、FEC 恢复调用和 PLC，按实际包时长处理，最多 120 ms；接收负载另有 1275 字节上限。FEC 调用成功不代表包内确实有冗余，没有可用冗余时 libopus 会执行 PLC。调用方必须按时间顺序恢复缺失间隔，再正常解码后续包。[libopus 编码接口](https://opus-codec.org/docs/opus_api-1.5/group__opus__encoder.html)、[解码接口](https://opus-codec.org/docs/opus_api-1.5/group__opus__decoder.html)、[编码控制](https://opus-codec.org/docs/opus_api-1.5/group__opus__encoderctls.html)。
- `Sender` 把相邻两帧 PCM 组合为一个 20 ms Opus 包；队列容量和过期阈值均为 60 ms，实际发送 tick 每次最多产出一包。丢样后时间戳按样本数跳进，不从零开始；为未来 RTP 层提供 48 kHz timestamp offset（8 kHz sample index × 6），实际随机初始 RTP timestamp、SSRC 与序号由后续 WebRTC 适配器负责。

## 构建与验证

普通构建不要求 libopus，也不会假装具备编解码能力。状态接口新增 `codec.available` / `codec.version`，与远程端到端 `available` 分开。

安装好 libopus 1.5.2 开发依赖后，可执行：

```sh
DJONEHUB_REMOTE_OPUS=1 ./scripts/build-macos.sh
go test -tags remoteopus ./internal/remotemedia/...
go test -race -tags remoteopus ./internal/remotemedia/... ./internal/callcontrol ./cmd/djonehub-macos
swift test --package-path macos/DJOneHubAudioHost
```

本批已通过全量普通 Go 测试、带真实 Opus 的上述竞态测试、Swift 宿主编译和 4 项 Swift 测试。验证覆盖 Go/Swift 共享协议样例、分帧与采样时钟跳跃、队列容量/过期/部分消费、请求边界与 token、双向 WebSocket、旧 generation、真实有损编解码、40/120 ms 包、PLC/FEC 调用与 PCM→Opus→PCM 数据通路。

Opus 开发构建当前动态链接本机 libopus；正式分发脚本仍不启用该 tag。可复现静态构建、源代码校验和、许可证随包分发和干净 Mac 启动验证要在开启正式能力前完成。

## 下一批仍需完成

1. Pion PeerConnection、SDP/ICE、DTLS/SRTP、Opus 协商和实际 RTP 接入；目前没有互联网媒体连接。
2. SSRC/序号/时间戳展开、乱序与去重、按截止时间播放的接收抖动缓冲、有限 PLC、FEC 调度，以及独立时钟间的长期漂移补偿。当前提供的 codec 调用不能替代这些机制。
3. Coordinator 授权远程 owner、本机凭证分发、UAC ready 与 answer-ready/音频激活协同；端到端取消、静音与失联挂断。
4. iOS CallKit/PushKit、配对、公网信令、APNs、TURN，以及 P0 实际 iPhone、模块和 CoreAudio 缓冲参数验证。

当前自动化测试不代表真实电话音质、弱网表现或长通话稳定性已经达标。
