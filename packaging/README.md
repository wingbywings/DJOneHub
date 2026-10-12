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

## 远程 iPhone 接听

2026-10-12 / R11：源码仓库中的[部署、重启与扫码配对指南](../docs/REMOTE_OPERATIONS_GUIDE.md)是完整操作入口。
更新现有设备通常不需要重新配对；本 Mac 包也不会自动安装 iPhone App。手机需通过同一 Team/Bundle ID 的 Debug 签名覆盖安装，保留现有 sandbox APNs 配置。

发行包已启用 WebRTC/Opus，并静态包含固定版本的 Opus；使用者不需要安装 Homebrew。
公网 Relay、TURN、APNs 和 iOS App 仍需按远程部署文档配置。本包不包含个人凭据或 APNs 私钥。

已在本机完成 Hub enrollment 和 iPhone 配对后，使用保存的 profile 名称启动：

```sh
djonehub start --remote-profile home-xiaomi
```

启动器会将同一个私有 native-control 文件交给 Hub 和 AudioHost，只监听 `127.0.0.1:7575`。
首次启动或更新二进制后，macOS 可能要求允许读取钥匙串中的 Hub 凭据；请及时处理系统提示。
启动最多等待约一分钟，超时会停止本次进程并保留日志，处理授权后可重新启动。

配对配置管理可直接通过启动器执行：

```sh
djonehub remote help
djonehub remote status --profile home-xiaomi
```

每个模块还需要启用远程接听并完成语音初始化；同一模块启用 AI 接听时优先走 AI，不会通知远程 iPhone。

日常重启先执行 `djonehub stop`，再执行上面的远程启动命令。启动器同时启动 Hub 与 AudioHost，终端需要保持打开；旧版手工启动的两个进程要先在原终端分别 Ctrl+C，`stop` 不负责它们。配对窗口用另一个终端运行 `djonehub remote-ui`，默认 profile `default` 要改为实际的 `home-xiaomi`。

恢复、续租和 DTMF 等开关默认关闭，启动器不透传这些参数。专项测试的双终端启动方法见操作指南。R06 并行迁移、R07 AudioHost 恢复及完整真机矩阵未完成，不能把本包作为全部验收通过的稳定发布。

### 原生远程配对窗口

完成 Hub profile 登记后，运行 `djonehub remote-ui` 打开 Mac 配对管理窗口。输入已登记的
profile 名称（本线程为 `home-xiaomi`，不是默认 `default`），读取配置，选择授权模块，再生成一次性二维码。iPhone App 选择“扫描 Hub
配对二维码”，核对服务器和模块后确认；不支持扫码或拒绝相机权限时，仍可导入 CLI 生成的 JSON。
二维码短时有效，不应分享截图；切走 Mac 窗口时自动隐藏。

手机配对后刷新 Mac 安装列表，复制安装 ID，在管理网页“来电”页的“iPhone 远程接听”设置
中填写、保存。设置按模块隔离；AI 接听优先，通话中更改由服务端拒绝。Mac 窗口可撤销安装
身份；撤销后请关闭相应模块的远程接听，或改为新的安装 ID。Hub 首次 enrollment 仍通过 CLI
完成，不在网页传递密钥。窗口通过随包 Hub 二进制访问原有 Keychain，可能出现系统授权提示。

iPhone 的通话记录只保留最近 100 条，不包含录音；保存在受系统文件保护且排除备份的本机文件，
可清除已结束记录。App 退出时未结束的通话，下次启动显示“结束状态未确认”，不能用于自动回拨
或恢复通话。没有收到有效推送或信令的未接来电不会凭空出现在手机记录中。

## 当前限制

- 支持 macOS 13 Ventura 至 macOS 26 Tahoe；当前发行包仅支持 Apple Silicon，不支持 Intel Mac。
- 语音通话需要模块固件、内核和运营商支持 IMS/VoLTE；不同固件组合需单独验证。
- 仅监听本机地址，局域网中的其他设备无法直接访问管理网页。
- 本项目为非官方工具，与 DJI、Quectel、运营商及 eSIM 卡片厂商无隶属或授权关系。
- 使用短信、蜂窝数据和 eSIM 前，请确认运营商资费、漫游规则及当地法律要求。
