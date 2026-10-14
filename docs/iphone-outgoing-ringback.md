# iPhone 外呼等待提示音

## 结论

接通前听到的声音通常称为回铃音，也可能是运营商彩铃或语音提示。
它不是从对方 iPhone 上下载的音频。本项目的 iPhone App 是模块电话的
VoIP 客户端，真实声音需要沿“蜂窝网络 → 模块 → D4/UAC → Mac →
WebRTC → iPhone”传递。

当前链路没有接通前音频支持，不能直接获取真实回铃音；这不等于模块
在硬件上一定不支持。是否有真实接通前音频，需要在目标模块、固件和
运营商环境中验证。部分移远模块的社区资料描述了 alerting 阶段通过 UAC
输出网络早期媒体的情况，但不能据此保证本项目设备也有输出。

## 当前静音的原因

- `cmd/djonehub-macos/remote_session.go` 的 `Observe()` 只有在模块状态为
  `active` 时才报告 `CellularActive`。
- `internal/remotemedia/readiness.go` 的 `AnswerReady()` 和
  `internal/remotemedia/peer.go` 的 `Activate()` 要求蜂窝通话已接通。
  接通前媒体不会转发，单纯提前打开 iPhone 播放无法获得模块声音。
- `shared/DJOneHubPhoneCore/Sources/DJOneHubPhoneCore/Signaling.swift` 的
  `activated()` 要求已经收到 `awaiting_audio`。iPhone 的 WebRTC 音频因此
  也保持关闭，直到模块接通并且 CallKit 音频会话激活。

## 本次默认音实现

`ios/DJOneHubPhone/Sources/PhoneRingback.swift` 在内存中合成 450 Hz 单声道
PCM WAV，响 1 秒、静默 4 秒循环，音头和音尾使用 10 ms 渐变。

`PhoneRuntime` 仅在外呼等待状态且 CallKit 已激活音频会话时播放。
收到模块接通确认后，在启用 WebRTC 前停止；取消、结束、失败和
CallKit 音频失活也会停止。来电不播放。播放器不自行激活音频会话，
不启用麦克风，也不改变服务端的媒体门控。

这个声音表示 App 正在等待拨号结果，并不证明对方已经响铃。
本次仍无法播放真实彩铃或运营商的接通前语音提示。

## 后续接入真实声音

先在一次受控外呼中，按 `dialing → alerting → active` 记录状态，并在
接通前采集模块 D4/UAC 下行 PCM。若确有回铃、彩铃或语音提示，再增加
独立的早期媒体状态，开放模块到 iPhone 的单向音频，保持麦克风上行关闭，
由服务端显式通知客户端停用默认音。不能直接移除 `CellularActive` 门控，
否则会把未接通阶段误当成完整通话。

若模块在该阶段始终不输出音频，就继续使用默认音；不能通过静音 RTP
包或通话状态本身还原真实彩铃。

## 验证

2026-10-14：Xcode 在 iOS 27 模拟器上完成编译，`PhoneRingbackTests`
4 项测试通过，覆盖播放门控、等待阶段不重复启动、接通/取消/失败/音频
失活时停止、来电不播放、播放失败处理，以及 WAV 可解码和静默区间。
真实电话的音量、听筒/蓝牙路由与锁屏外呼仍需真机试听。

参考：[Apple 的 VoIP/CallKit 音频激活流程](https://developer.apple.com/documentation/callkit/making-and-receiving-voip-calls)
及[移远社区关于 UAC 与接通前回铃音的说明](https://forums.quectel.com/t/question-about-korean-local-ringback-tone-in-ec25efar08a05m4g-kr-firmware/57857)。
