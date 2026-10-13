# DJOneHub for macOS（Apple Silicon）

适用于搭载 Apple M 系列芯片的 Mac，以及大疆一代 4G 模块（USB `2ca3:4006`）。

## 安装（推荐）

1. 将下载的 ZIP 完整解压，不要只单独拖出其中某个文件。
2. 打开“终端”，输入 `cd `（后面留一个空格），把解压得到的文件夹拖入终端，然后按回车。
3. 执行一次安装命令：

   ```sh
   ./install
   ```

   安装过程可能要求输入 Mac 管理员密码。输入密码时终端不会显示圆点或星号，这是正常现象。

安装完成后，可以在终端的任意目录执行：

```sh
djonehub start
```

程序会自动打开 `http://127.0.0.1:7575`。启动程序的终端需要保持打开；按 `Control+C` 即可停止。

也可以在另一个终端的任意目录执行：

```sh
djonehub stop
```

## 免安装使用

如果不想安装，也可以一直保留解压后的文件夹，在该目录执行：

   ```sh
   ./djonehub start
   ```

## 常用命令

```sh
djonehub status       # 查看状态
djonehub logs         # 查看实时日志
djonehub open         # 重新打开管理网页
djonehub start --demo # 不连接硬件，打开演示界面
```

## macOS 阻止打开时

本软件目前没有 Apple Developer ID 签名。首次启动如果被 macOS 阻止，请打开“系统设置 → 隐私与安全性”，确认仍要打开。

如果系统仍提示文件损坏，可在当前目录执行：

```sh
xattr -dr com.apple.quarantine ./djonehub ./bin ./lib
./djonehub start
```

请只对从可信发布页面下载并核对过 SHA-256 的文件执行该命令。

## 日志

程序日志保存在：

```text
~/Library/Logs/DJOneHub/djonehub.log
```

终端只显示启动、停止和错误摘要，不会持续刷出底层 USB 日志。

## 语音通话（实验性）

语音通话会按所选模块的物理 USB 端口精确绑定。首次使用时，请在“来电监控”页面
依次确认模块初始化和语音运行时安装；两项操作都不会自动执行。初始化会保存原配置，
写入后回读验证，失败时自动恢复。语音运行时从固定上游版本下载并校验 SHA-256，
不包含在发行包中。

通话接通后，DJOneHub 会自动启动模块侧音频路由和 Mac 音频宿主。macOS 首次使用
麦克风时会显示权限提示；拒绝权限后可在“系统设置 → 隐私与安全性 → 麦克风”中修改。
网页可控制静音和 WAV 录音。完成真实模块呼入、呼出与拔插验收前，此功能仍属于实验性。

## AI Voice Agent（实验性）

发行包已包含 Agent 所需的本地音频宿主，`djonehub start` 会自动启动它。当前支持
Qwen/OpenAI 原生实时语音，以及 Qwen/OpenAI STT → MiniMax LLM/TTS 级联。

云端 API Key 不会写入配置文件。请在启动命令所在的终端设置所需密钥：

```sh
export DASHSCOPE_API_KEY='...' # Qwen
export OPENAI_API_KEY='...'    # OpenAI
export MINIMAX_API_KEY='...'   # MiniMax 级联
djonehub start
```

启动后可查询 Provider 状态，并通过本机 API 启用 Qwen：

```sh
curl http://127.0.0.1:7575/api/voice-agent/status
curl -X PUT http://127.0.0.1:7575/api/voice-agent/config \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true,"provider":"qwen","voice":"Cherry","instructions":"你是电话客服，请简洁回答。","auto_answer_delay_ms":1200}'
```

受控电话工具、审计日志和自动接听固定开启，审计内容自动脱敏固定关闭。进入管理页面的“来电”标签即可配置 Provider、模型、STT 降级、
工具确认、审计和脱敏，并查看实时转写。Profile 保存在
`~/Library/Application Support/DJOneHub`，但不包含 API Key。OpenAI、MiniMax 和
自定义模型/Endpoint 的完整示例请参阅源码仓库的 `docs/voice-agent-first-batch.md`。

## 远程 iPhone 接听与 Web 配对

2026-10-13 / Web 配对版：完整步骤见发行包内的 [Web 操作指南](REMOTE_WEB_OPERATIONS_GUIDE.md)。
新包已包含 WebRTC/静态 Opus，普通 `djonehub start` 同时启动 Hub 和 AudioHost，并打开
`http://127.0.0.1:7575/remote/`。注册、授权文件导入、二维码、已配对手机与各模块目标均在 Web 操作。

旧 profile 可在网页导入一次，例如 `home-xiaomi`；不覆盖 Keychain、不重新登记手机。
选择已登记配置并保存重启选择后，后续 `djonehub start` 自动连接该配置。通话/恢复未完成时拒绝切换。
`djonehub open` 可重新登录本机管理；直接访问页面可用自行设置的管理密码。

新增 Relay 管理站点需要独立部署。先升级 native Relay 到 schema 3，再启动管理服务；
部署说明随包提供为 `REMOTE_RELAY_WEB_ADMIN_DEPLOY.md`。公网原生 Relay、TURN、APNs 与 iOS App
沿用原有配置。本包不包含个人凭据，也不会自动安装或清理 iPhone App。

`djonehub start --remote-profile home-xiaomi` 保留兼容：读取旧身份、加入 Web 列表并保存重启选择。
`remote-ui` 现在打开 Web 页，独立 Mac 配对窗口已迁出主要发行包；CLI `remote help/status/pair/revoke`
保留故障处理用途。新 Web 的目标手机是按模块授权过滤的下拉选择，不需要粘贴安装 ID。

每个模块仍需完成语音初始化；启用 AI 时只走 AI，不通知远程 iPhone。网页保存远程目标不会自动关闭 AI。
macOS 更新后二进制访问原 Keychain 可能需要系统授权；后台管理任务显示进度和明确结果，超时需先核对，
不能反复消费同一授权。iPhone 扫码/文件确认和本机最近 100 条记录保持现有行为。

代码与自动化验证已完成，浏览器全流程、真实安装和通话验收仍待核对。恢复、续租和 DTMF 开关默认关闭；
R06 并行迁移、R07 AudioHost 故障恢复及完整弱网/真机矩阵仍未完成。

## 当前限制

- 支持 macOS 13 Ventura 至 macOS 26 Tahoe；当前发行包仅支持 Apple Silicon，不支持 Intel Mac。
- 语音通话需要模块固件、内核和运营商支持 IMS/VoLTE；不同固件组合需单独验证。
- 仅监听本机地址，局域网中的其他设备无法直接访问管理网页。
- 本项目为非官方工具，与 DJI、Quectel、运营商及 eSIM 卡片厂商无隶属或授权关系。
- 使用短信、蜂窝数据和 eSIM 前，请确认运营商资费、漫游规则及当地法律要求。
