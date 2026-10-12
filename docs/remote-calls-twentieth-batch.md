# 第二十批：R10 AudioHost 事件订阅

日期：2026-10-12。分支 `codex/remote-iphone-callkit`，未提交；没有更新运行中的 Hub 或 AudioHost，也未触发真实通话。

## 实现

在受 bootstrap Bearer 认证的 loopback native-control 服务增加 `/events` WebSocket。通道只发送 `changed` 和单调 revision，不包含号码、通话绑定、配置或任何凭据。每次建立连接先通知客户端读取完整配置，避免漏掉断线期间的状态变化；每次重连从私有 bootstrap 文件重新读取端口和 token。

Hub 在来电/接通/挂断、模块附加/离线/重新枚举、原生媒体会话建立、音频路由就绪变化和静音/录音意图变化时通知。稳定 CLCC 轮询不会每次生成事件。多个模块共享服务的事件源；收到通知后 AudioHost 重新选取当前模块并通过原有配置校验获取状态，事件本身不能启用音频。

每个订阅者最多保留一个待处理提示；服务端最多 8 个订阅者、最多约每 50 ms 推送一次。慢客户端不会阻塞通话工作，也不会积压历史事件。保留 Ping/Pong 和读写超时；服务关闭时同时关闭被 WebSocket 接管的连接。浏览器 Origin、非 loopback 来源、错误凭据和缺少 Host ID 均不能订阅。

AudioHost 将状态刷新与远程健康上报分开：

- 事件到达立即安排刷新，使用单元素队列合并密集通知；完整配置检查串行执行。
- 事件在线且远程音频稳定时，每 5 秒进行一次兜底检查。
- 事件在线的空闲/本机音频模式每 2 秒检查，保持在旧状态注册的 5 秒有效期之内。
- 订阅未建立/断开、音频仍在启动或有错误时，每 500 ms 检查；订阅重试退避 1–5 秒。
- 远程原生健康上报始终独立每 500 ms 执行，不受配置事件稀疏影响；上报仍绑定 UUID/generation，过期请求不能停止下一通音频。
- 不支持事件的旧 Hub 或未配置 native-control 的宿主保留 500 ms 轮询兼容路径。现有 PCM 与 CallKit 音频激活门控不改变。

此前完成的 CLCC 自适应轮询仍有效：稳定远程模式目标 1 秒，过渡期 250 ms，AI 周期独立。事件通知消除了 AudioHost 在稳定远程通话时每 500 ms 获取整套设备/通话/配置的需要，不取消必要的健康心跳。

## 验证

- Hub race 回归通过，涵盖事件凭据/Origin/来源限制、首次同步、重订阅、停机关闭、订阅上限与一万次通知合并，以及稳定 CLCC 不产生重复事件、接通/挂断产生通知。
- Swift AudioHost/RemoteAudio 构建及 11 项常规测试通过，包括事件形状/字段/序号限制和健康有效期所需的刷新预算；原有 PCM 测试通过。
- 可选 Go/Swift 互通测试通过：临时 bootstrap + 实际 Go native-control listener + Swift URLSession WebSocket，连续接收递增事件并取消订阅。没有访问硬件配置、麦克风或 UAC。

日志：`/tmp/djonehub-r10-go.log`、`/tmp/djonehub-r10-swift.log`、`/tmp/djonehub-r10-native-interop.log`。

互通复测命令：

```sh
DJONEHUB_NATIVE_EVENTS_INTEROP=1 go test -race -tags remoteopus ./cmd/djonehub-macos -run '^TestNativeEventsSwiftInterop$' -v
```

尚未升级真实 AudioHost。需要实际来电、挂断、模块重枚举和长时间通话，验证事件刷新延迟与健康上报没有回退；不能用本地测试代替真实 USB/通话负载验收。R11 产品入口仍待后续实施；R06 并行迁移、R07 宿主恢复仍保持未完成。
