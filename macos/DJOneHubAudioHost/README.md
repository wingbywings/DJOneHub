# DJOneHubAudioHost

macOS 双向通话音频宿主。它从 DJOneHub 后端读取当前活动通话和精确 USB
`vendor_id`、`product_id`、`location_id`，只绑定对应模块的 UAC 输入/输出。
人工模式下，它连接本地麦克风和扬声器；Agent 模式下，它将模块上行的 8 kHz
PCM 通过本机 WebSocket 发送给 Go 后端，并把模型合成的 PCM 写回模块。

构建验证：

```sh
swift build --package-path macos/DJOneHubAudioHost -c debug
```

先启动 Go 后端，再开发运行音频宿主：

```sh
swift run --package-path macos/DJOneHubAudioHost \
  DJOneHubAudioHost --base-url http://127.0.0.1:7575
```

多模块环境会自动选择唯一的活动通话，也可使用 `--device-id <Device ID>` 固定模块。
首次运行需要允许麦克风权限。录音文件保存在
`~/Library/Application Support/DJOneHub/recordings`。

云端 API Key、模型和 Voice Agent Profile 均由 Go 后端管理，音频宿主不读取或保存
这些密钥。它每 500 ms 拉取媒体模式；在人工/Agent 模式之间切换时会重建当前媒体
通道。Agent WebSocket URL 包含后端生成的临时 token，只绑定本机后端，并且同一设备
只允许一个 Agent 媒体会话。

Agent 媒体桥断线后会按最长 8 秒的指数退避自动重连。Profile revision 改变时会主动
重建媒体会话；收到后端的 `speech.started` 事件时会清空尚未写入模块的 Agent PCM，
让来电方可以打断 AI 播报。

制作发行包时无需单独复制该产物，根目录的 `scripts/package-macos-arm64.sh`
会执行 release 构建并将其打包为 `bin/djonehub-audio-host`。

`CModemBridge`、`CUACProbe` 和 MaVo 音频服务来自项目参考实现，保留其 MIT
许可标识及原始实现注释。
