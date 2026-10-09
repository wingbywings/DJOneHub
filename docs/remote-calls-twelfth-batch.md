# 远程接听：第十二批 iOS 在线 App 与 TURN 接入

日期：2026-10-09。分支：`codex/remote-iphone-callkit`。

本批新增可编译的原生 iOS App，包含配对文件导入、Keychain、鉴权 WSS、CallKit、WebRTC Opus 和本地恢复入口；同时将 TURN 临时凭据接入 Relay、Mac 和 iPhone。**这仍是前台在线联调版本，APNs/PushKit 后台唤醒尚未实现；没有完成真实 iPhone、真实电话模块及公网验收。**

## 工程与构建

- `ios/DJOneHubPhone/DJOneHubPhone.xcodeproj`：可直接用 Xcode 打开，默认 Bundle ID 为 `org.djonehub.phone`，最低 iOS 17。
- `project.yml`：XcodeGen 源配置。新增文件或更改依赖后运行 `xcodegen generate --spec ios/DJOneHubPhone/project.yml`。
- `shared/DJOneHubPhoneCore`：配对/刷新、信令校验、通话状态机，可独立运行 macOS Swift 测试。
- WebRTC 使用 stasel 分发的固定 M154 XCFramework，版本及 revision 保存在 `Package.resolved`，下载校验值由其 Package.swift 固定。它是第三方二进制分发；许可证说明见 `THIRD_PARTY_NOTICES.md`。

选择自己的签名 Team 和 Bundle ID 后才能部署真实 iPhone；本批只进行了无签名构建和模拟器测试，没有代替用户申请证书、注册 App ID 或上传 App。App 声明 audio 和 voip 后台模式以支持 CallKit 来电报告；未注册 VoIP Push、未添加 APNs entitlement，不宣称后台待机来电可用。

```sh
swift test --package-path shared/DJOneHubPhoneCore --disable-sandbox
xcodebuild -project ios/DJOneHubPhone/DJOneHubPhone.xcodeproj \
  -scheme DJOneHubPhone -sdk iphonesimulator \
  -destination 'generic/platform=iOS Simulator' CODE_SIGNING_ALLOWED=NO build
```

## 配对与凭据

在 Mac 使用第十批的 `remote pair --profile ... --modules dji-... --out ...` 生成 5 分钟有效的 JSON 文件，通过选定的安全交付方式保存到 iPhone，再点击“导入 Hub 配对文件”。不能重复覆盖现有配对。服务地址仅接受 HTTPS origin；不接受 userinfo、路径、查询参数或明文 HTTP。

App 创建 Ed25519 安装密钥，沿用共享签名包，向 Relay 兑换安装身份。完整安装凭据保存在本机 Keychain generic password 中，service 为 `org.djonehub.phone.identity.v1`，不进行 iCloud 同步，使用 `AfterFirstUnlockThisDeviceOnly`。当前每个 App 实例只管理一份安装身份。

事务流程：

```text
不存在 → pairing_pending → ready
ready → refresh_pending → ready（新凭据）
```

pending 在消费配对或刷新凭据之前落盘。网络响应丢失、最终保存失败或进程退出后，保留不确定状态并禁止重试旧凭据。刷新先验证身份、模块范围及版本保持一致，再保存完整新凭据；Keychain 保存完成后才通过 WSS reauth 更新连接。

界面只显示安装 ID 和公钥指纹，不显示 token、私钥或 TURN password。不确定状态下，应先按第十批的指纹查找流程在 Relay 撤销旧身份，再使用 App 的“清理本机配对凭据”确认入口删除本机项并重新配对。清理本机项本身不会撤销 Relay 身份。

真实 iPhone Keychain 的签名、访问保护、锁屏及卸载重装行为仍需硬件验收。测试使用内存 Vault；模拟器宿主启动时会访问其隔离 Keychain，没有读取或改写 Mac 登录 Keychain 或真实手机凭据。

## 接听顺序和隔离

1. 用户点击连接，App 先取得麦克风权限，再以安装 access token 建立 WSS，核对协商子协议及 welcome 身份/Hub。
2. 接收完整、符合模块权限的邀请后，立即报告 CallKit；UUID、Hub epoch、模块、安装身份、generation 构成完整绑定。
3. 收到 offer 后，经已认证 ICE API 获取本通 TURN 配置，建立只含音频的 Peer；提前到达的 ICE 暂存在有界队列中。没有视频采集、数据通道、录音或系统最近通话写入。
4. 用户接听后，等待 CallKit 报告成功和 WebRTC transport 就绪，向 Hub 发 answer 命令；不重发 ATA。
5. Hub 报告 `awaiting_audio`（蜂窝/宿主就绪）后，App 才 fulfill CallKit 接听动作。
6. 收到系统 `didActivate` 后，开启 WebRTC 手动音频，并发送带递增 sequence 的 `audio_active`。在此之前音轨和音频设备保持关闭。
7. 拒接/挂断使用最新 revision。发送、接听动作、音频激活、信令心跳和续期均有超时；失败关闭连接，由 Hub 的既有 Session 收尾。

系统回调和 SDP/ICE 异步结果都再次核对绑定。已经结束的 UUID 不接受重邀；旧回调不能结束或激活下一通。空闲断线不会补发旧通话。普通 `peer_closed` 只结束当前电话，iPhone WSS 继续等待下一次邀请。系统 reset、媒体断线和不可恢复协议错误会断开，需要用户重新连接。

手动音频顺序依据 [Apple CallKit didActivate](https://developer.apple.com/documentation/callkit/cxproviderdelegate/provider%28_%3Adidactivate%3A%29) 和 [WebRTC RTCAudioSession](https://webrtc.googlesource.com/src/+/e8d5724cc5ee0b593a996477c7230bba3dc4d7bc/sdk/objc/components/audio/RTCAudioSession.h)。真实 CallKit 界面、蓝牙/听筒、系统打断仍未由模拟器协商测试替代。

## 网络与 TURN

Relay 新增可选 `serve --turn-config /absolute/private/turn-issuer.json`。配置包含 TURN URLs、共享 secret 和 relay_only。文件要求当前用户拥有的私有普通文件，拒绝符号链接/多硬链接；不从命令行传 secret。

新增 `POST /v1/ice`：Bearer 鉴权，body 为 `{"module_id":"dji-..."}`。每次读取当前身份与模块权限，撤销身份不能再获取凭据。未配置 TURN 时明确返回 `enabled:false`；已配置但请求失败时，客户端不会偷偷回退到直连。

凭据按 coturn REST 格式签发，使用随机临时用户名、到期时间和 HMAC-SHA1 proof，响应为 no-store。主密钥不进入 Mac/iPhone。两端都验证 URL、时限和格式；支持 TURN/UDP、TURN/TCP、TURN/TLS 的配置以及强制 relay-only。

凭据期限固定为 4 小时，当前未实现通话中 ICE 重启/续租凭据更换。两端在凭据到期前 30 秒结束本通。已有凭据的撤销边界、coturn 配额、端口和配置模板见 [部署说明](../deploy/remote-call/README.md)。

Hub 在线信令使用 Trickle ICE：生成 SDP 后立即发送，候选地址经 WSS 持续补充，两端都暂存早于 SDP 到达的候选。不等待全部 ICE 服务器完成探测，避免一个不响应的 UDP 端点阻塞 offer。该优化不放宽接听、DTLS 或音频激活门禁。

iOS Opus 发送上限设置为 24 kbps，使用系统 voiceChat 音频路径和约 20 ms 首选 IO buffer。参数设置不等于实际时延保证；网络切换、自适应、时钟漂移与长通话仍在后续验证范围内。

## 当前验证

- Swift PhoneCore 14 项测试：接听/音频门禁、重复和迟到事件、超时、ICE 数量和消息大小、身份/模块绑定、配对与刷新日志、重启恢复、TURN 格式及模块授权。
- iOS 模拟器原生测试：两个真实 WebRTC Peer 完成 Opus SDP、ICE 和 DTLS 协商，且未收到 CallKit 激活时音频仍关闭。测试不发送真实麦克风音频。
- iOS 模拟器构建及真机 SDK 无签名构建通过；尚未安装真实 iPhone。
- Go 新增 TURN 配置、私有文件、权限/撤销及 HTTP 客户端测试；Trickle ICE 直连及真实本机 TURN/UDP、TURN/TCP 强制中继下完成双向 Opus 传输/解码。中继测试断言双方选中的都是 relay candidate；不响应 ICE 服务器测试验证 offer 不被完整候选采集阻塞。
- 全仓库 Go 回归及受影响包竞态回归通过。

2026-10-09 Opus 兼容性更新：Mac 构建脚本支持 libopus 1.5.2 和 1.6.1；本机使用 1.6.1 完成后端构建，以及 `remotemedia/...`、`remotecall`、`cmd/djonehub-macos` 的真实编解码、PLC/FEC、WebRTC/TURN 双向音频竞态回归。编解码仍使用原有公开 API、8 kHz 单声道和 20 ms 帧；iOS 继续使用固定 WebRTC SDK，无需另装 Homebrew libopus。实际加载版本通过 `codec.version` 返回。

这里的 Go TURN 测试使用 Pion TURN 服务端，不是已经部署的 coturn；原生 iOS 测试使用 SDK 双 Peer，不等于已经验证 iPhone ↔ Pion ↔ UAC ↔ 蜂窝全链路。TURN/TLS 真实证书、公网 NAT/NAT64、受限网络和硬件验收仍未完成。

下一步为 APNs/PushKit 唤醒、离线邀请生命周期、真实签名和端到端电话验收，以及网络切换和长通话稳定性。
