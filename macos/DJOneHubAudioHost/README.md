# DJOneHubAudioHost

macOS 双向通话音频宿主。它从 DJOneHub 后端读取当前活动通话和精确 USB
`vendor_id`、`product_id`、`location_id`，只绑定对应模块的 UAC 输入/输出。

开发运行：

```sh
swift run DJOneHubAudioHost --base-url http://127.0.0.1:7575
```

多模块环境会自动选择唯一的活动通话，也可使用 `--device-id <Device ID>` 固定模块。
首次运行需要允许麦克风权限。录音文件保存在
`~/Library/Application Support/DJOneHub/recordings`。

`CModemBridge`、`CUACProbe` 和 MaVo 音频服务来自项目参考实现，保留其 MIT
许可标识及原始实现注释。
