# 第十四批：弱网音频、采样时钟和远程发行包

日期：2026-10-10。分支：`codex/remote-iphone-callkit`。
本批不变更 Relay/iOS 信令协议，不需要因本批音频改动重新安装 iPhone App。

## 实现

- **R01**：接收缓冲从初始 60 ms 按 RTP 到达抖动调整至 40–120 ms；连续新包迟到会重新建立播放时间基准，防止持续延迟变化后永久静音。SSRC、generation、重放和已解码时间范围检查仍保留。降低目标延迟时通过时钟微调逐步收敛。
- **R02**：Go 接收端和 Swift UAC 短队列分别按队列占用反馈进行有状态线性插值，补偿独立采样时钟。校正上限 ±1000 ppm，速率渐变；UAC 部分写入只推进实际接收的样本数。调度严重停顿仍执行有界清理，不尝试播放积压音频。
- **R03**：解析实际发送 SSRC 对应的 RTCP Reception Report。丢包反馈调节 Opus FEC 预估；连续丢包伴随抖动或 RTT 上升时逐步降低码率，稳定后再上调，范围 12–24 kbps。保留 5 秒调节间隔和 20 秒稳定窗口。诊断新增 feedback 的报告数、码率、丢包、jitter、RTT 及调整次数。
- **R10（部分）**：远程空闲与稳定通话每秒 CLCC 检查；振铃、拨号和接听/结束中每 250 ms 检查。AI/本地通话保留既有周期，活动通话使用不可变路由快照。状态接口保留小数秒并增加毫秒字段。AudioHost 的事件订阅尚未实现。
- **R13**：发行构建增加 `remoteopus`，固定并校验官方 Opus 1.6.1 源码、静态链接并携带许可证。启动器支持 `start --remote-profile NAME`，将相同私有 native-control 文件交给 Hub 和 AudioHost；显式监听 loopback。新增配对 CLI 透传、参数检查和约一分钟启动等待，超时清理本次进程。

接收时钟和反馈参考 [RFC 3550](https://www.rfc-editor.org/rfc/rfc3550.html)；Opus RTP 使用 48 kHz 时间戳，媒体 PCM 仍为模块所需的 8 kHz 单声道。码率策略是本项目的保守控制策略，并非声称已经实现完整带宽估计器。

## 自动化验证

- `go test -race -tags remoteopus ./internal/remotemedia ./internal/remotecall ./cmd/djonehub-macos`：媒体、会话及 Hub 联合竞态回归通过。
- Go ±500 ppm 各三小时加速时钟仿真；真实 Receiver 各一小时加速仿真，要求无 PLC、无迟到、无重缓冲且队列有界。
- 模拟网络延迟阶跃增加 100 ms、独立输出时钟和乱序到达，验证恢复和防重放。
- RTCP 测试覆盖真实 SRTCP 报告、SSRC 隔离、重复序号、NTP 回绕、配置失败、拥塞节奏和恢复。
- Swift 9 项测试通过，包括 ±500 ppm 各五分钟加速仿真、部分 UAC 写入及状态清理。
- 新增 `scripts/check-macos-package.py`，检查发行包内的库依赖、签名、许可证、启动器参数及独立 demo 进程中的 Opus 可用性；不访问 USB/Keychain。

加速仿真只验证队列及校正算法，不能替代三小时真实通话或完整弱网矩阵。仍需真实双方连续语音、收尾状态和主观听感验收。

## 运行与发布状态

开发版音频改动已加载。用户允许 macOS 钥匙串访问后，Hub 的远程连接恢复 `online`，新版 AudioHost 已注册，Opus 1.6.1 可用。用户完成复测并确认“延迟很小，可以接受”；本次已通过实际网络的延迟主观验收，详细观测见下文。
自适应 CLCC 已进入源代码和发行包；当前运行的开发 Hub 是本批音频测试构建，尚未为轮询改动再次重启。

发行包 `dist/release/DJOneHub-macOS-arm64-remote-adaptive-20261010.zip` 已生成，附 SHA-256 文件；`check-macos-package.py` 自检通过。已验证压缩包校验和，不含 JSON 凭据或 p8 私钥。独立 demo 验证不代替真实安装升级、旧系统兼容性或发布签名验收。

本批未完成 R04–R09、R11–R12、R14，以及 R10 的宿主事件订阅。Mac 打包支持也不代表 production APNs、Apple Developer ID、公证或 iOS 分发已经验收。


## 2026-10-10 真机复测记录

用户反馈原文：“延迟很小，可以接受”。服务端记录两次成功接听，均清理成功：

- `703f5077-591a-4360-949b-afd2540676b2`：来电生命周期约 71 秒，远程挂断；下一通已覆盖其内存媒体快照，不能把下一通的统计归给这通。
- `3da04896-25b5-4f4c-b163-a2e4496aaa9b`：来电生命周期约 19 秒，蜂窝端结束；最后保留的媒体统计如下。生命周期含振铃和准备时间，不等于有效语音时长。

| 指标 | 最后一通快照 |
| --- | --- |
| 下行 PCM / Opus | 1,127 个 10 ms 帧 / 562 个 20 ms 包 |
| 下行丢帧 | 0 |
| 上行收到 / 解码 | 551 / 547 包 |
| 上行迟到 / 拒收 / 重复 | 全部 0 |
| PLC / 静音填充 / 重新缓冲 | 全部 0 |
| 接收目标延迟 / 到达抖动 | 60 ms / 2.66 ms |
| RTCP 丢包 / 抖动 / RTT | 0% / 3.67 ms / 15.20 ms |
| Opus 码率 | 16 kbps |
| 上行采样时钟校正 | +88.05 ppm |
| 收尾 | ended，cleanup_failed=false；Hub online，Host 已停止本次音频 |

RTT 是网络往返时间，不是端到端语音时延。以上证明本次网络和样本中无所列异常，
不能替代受控弱网、三小时真实通话、Wi-Fi/蜂窝切换或 TURN 续租验收。

下一批恢复实现须协调 R04/R05：当前 PhoneMedia 对 disconnected 立即失败，
RunSignaledCall 对 WSS client.Done 直接收尾，PhoneRuntime 接受 offer 会创建新 Peer。
不能只延迟 iOS 报错便宣称支持切网；须先定义保留同一通话的恢复期限、协商版本和
重连身份绑定，再同时处理旧 offer/answer/ICE、命令重放和恢复超时的蜂窝清理。
