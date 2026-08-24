# DJOneHub 中文编译与运行指南

本文面向需要从源码编译、调试或制作 DJOneHub macOS 发行包的开发者。普通用户如只想使用软件，建议直接下载项目 Release 中的 `macOS-arm64` ZIP，并参阅根目录的 `README.md`。

## 1. 项目与构建方式概览

DJOneHub 是一个面向大疆第一代 4G 模块的 macOS 本地管理程序，主要技术组成如下：

| 目录/文件 | 作用 |
| --- | --- |
| `cmd/djonehub-macos/main.go` | macOS 主程序入口、HTTP API、设备状态、短信、eSIM、网络与流量功能 |
| `cmd/djonehub-macos/usbat_darwin.go` | 通过 CGO 和 libusb 访问大疆模块 USB AT 接口 |
| `cmd/djonehub-macos/web/` | 使用 `go:embed` 编译进二进制的原生管理页面 |
| `internal/voiceagent/` | Provider 无关的实时语音、流式 STT、级联 LLM/TTS、重采样和 Qwen/OpenAI/MiniMax 适配器 |
| `internal/` | APDU 仲裁、设备后端、配置、eSIM、调制解调器和 SIM AID 等其他内部实现 |
| `macos/DJOneHubAudioHost/` | Swift Package；负责 CoreAudio/UAC、人工麦克风通话及 Go ↔ Swift Agent PCM 媒体桥 |
| `pkg/` | 日志、MBIM、短信 PDU 编解码等共享包 |
| `third_party/` | 由 `go.mod` 的 `replace` 指令引用的本地第三方源码 |
| `scripts/build-macos.sh` | 依赖本机 libusb 的日常开发构建脚本 |
| `scripts/package-macos-arm64.sh` | 构建并打包自带 libusb 的 Apple Silicon 发行包 |
| `packaging/` | 发行包启动器、安装器和发行说明 |

项目没有需要单独构建的 Vue、React 或 Node.js 前端。`cmd/djonehub-macos/web/` 中的 HTML、CSS 和 JavaScript 会随 Go 程序一起嵌入二进制。语音通话另有一个必须单独编译的 Swift 音频宿主；发行打包脚本会自动完成这一步。

需要特别注意：

- 根模块当前仍名为 `github.com/iniwex5/vohive`，这是现有源码的真实导入路径，不影响 DJOneHub 构建，不要仅为编译而修改它。
- `go.mod` 声明的 Go 版本为 **1.26.3**。
- macOS 主程序依赖 CGO 和 libusb，不能用 `CGO_ENABLED=0` 代替正常构建。
- 接听、拨号和 Voice Agent 媒体链路依赖 `DJOneHubAudioHost`；只构建 Go 二进制仍可使用短信、eSIM、AT 等功能，但没有本机双向通话音频。
- `third_party/` 只包含部分本地替换依赖；第一次构建仍可能需要联网下载其余 Go 模块。
- 当前正式打包流程只支持 Apple Silicon（arm64），Intel Mac 尚未发布和真机验证。

## 2. 环境要求

### 2.1 系统与硬件

- Apple Silicon Mac（M1、M2、M3、M4 或后续 Apple 芯片）
- macOS 13 Ventura 或更新版本
- 制作真机功能验证时，需要大疆第一代 4G 模块、支持数据传输的 USB-C 线和可用 SIM/eUICC 卡片
- 只运行演示模式时不需要连接硬件

确认系统架构：

```sh
uname -m
```

发行打包要求输出为：

```text
arm64
```

### 2.2 开发工具

必须安装：

- Go 1.26.3 或兼容的更新版本
- Xcode Command Line Tools（提供 `clang`、macOS SDK 和签名工具）
- Swift 6（随当前 Xcode Command Line Tools/Xcode 提供）
- `pkg-config`
- libusb 1.0 开发文件
- Git

安装 Xcode Command Line Tools：

```sh
xcode-select --install
```

如果使用 Homebrew，可安装构建依赖：

```sh
brew install go pkg-config libusb
```

如果已经安装 Go，请先确认版本满足 `go.mod`：

```sh
go version
```

Go 支持按 `go.mod` 自动下载所需工具链，但该方式要求能访问 Go 模块代理，并且 Go 模块缓存目录可写。网络受限环境建议预先安装 Go 1.26.3 或更新版本。

### 2.3 环境自检

在项目根目录执行：

```sh
go version
go env GOOS GOARCH CGO_ENABLED
xcode-select -p
clang --version
swift --version
pkg-config --modversion libusb-1.0
```

在 Apple Silicon Mac 上，关键结果应满足：

- `GOOS` 为 `darwin`
- `GOARCH` 为 `arm64`
- `CGO_ENABLED` 为 `1`
- `swift --version` 能正常输出 Swift 6 工具链信息
- `pkg-config` 能输出 libusb 版本，而不是 `command not found` 或 `Package libusb-1.0 was not found`

如 Homebrew 已安装 libusb，但 `pkg-config` 仍找不到它，可执行：

```sh
export PKG_CONFIG_PATH="$(brew --prefix libusb)/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
pkg-config --cflags --libs libusb-1.0
```

## 3. 获取依赖与运行测试

进入项目根目录后下载尚未缓存的 Go 依赖：

```sh
go mod download
```

运行全部测试：

```sh
go test -mod=mod ./...
swift build --package-path macos/DJOneHubAudioHost -c debug
```

测试 `cmd/djonehub-macos` 时同样会编译 libusb/CGO 代码，因此缺少 `pkg-config` 或 libusb 会导致该包构建失败。不要使用 `CGO_ENABLED=0` 绕过：当前无 CGO 替代文件只用于有限的平台占位，并不足以构建完整主程序。

需要查看具体测试过程时，可使用：

```sh
go test -mod=mod -v ./cmd/djonehub-macos
go test -mod=mod -v ./internal/...
go test -mod=mod -v ./pkg/...
go test -mod=mod -v ./internal/voiceagent/...
```

## 4. 日常开发构建

### 4.1 使用仓库脚本（推荐）

```sh
./scripts/build-macos.sh
```

脚本会执行启用 CGO 的 macOS 本机构建，并将结果写入 `dist/`：

```text
dist/djonehub-macos-arm64
dist/djonehub-macos
```

该脚本只构建 Go 主程序。需要调试语音通话或 Voice Agent 时，还要构建第 4.3 节的 Swift 音频宿主。

在 Intel Mac 上文件名中的架构会随 `go env GOARCH` 变化，但 Intel 构建目前没有经过项目发布和真机验证。

### 4.2 等价的手动构建命令

Apple Silicon 上可执行：

```sh
export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-/opt/homebrew/lib/pkgconfig:/usr/local/lib/pkgconfig}"
CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build \
  -p 2 \
  -trimpath \
  -ldflags="-s -w" \
  -o dist/djonehub-macos-arm64 \
  ./cmd/djonehub-macos
```

开发构建会链接本机安装的 libusb。可以检查动态库引用：

```sh
otool -L dist/djonehub-macos-arm64
```

这类二进制适合本机开发，不适合直接复制给未安装 libusb 的用户。对外分发请使用第 7 节的发行打包脚本。

### 4.3 构建 Swift 音频宿主

在项目根目录执行：

```sh
swift build --package-path macos/DJOneHubAudioHost -c debug
```

产物通常位于：

```text
macos/DJOneHubAudioHost/.build/debug/DJOneHubAudioHost
```

也可以直接使用 `swift run` 编译并启动：

```sh
swift run --package-path macos/DJOneHubAudioHost \
  DJOneHubAudioHost --base-url http://127.0.0.1:7575
```

音频宿主支持 `--device-id <Device ID>` 固定多模块环境中的目标模块。省略时，它会从后端选择唯一的活动通话。

## 5. 运行与调试

### 5.1 无硬件演示模式

先用演示模式确认二进制和管理页面可正常工作：

```sh
./dist/djonehub-macos -demo
```

浏览器访问：

```text
http://127.0.0.1:7575
```

也可在另一个终端检查健康接口：

```sh
curl http://127.0.0.1:7575/api/health
```

演示模式只提供模拟状态、短信、AT 响应和 eSIM Profile，不会访问真实硬件，也不会发送短信或修改实体 eUICC。

按 `Control+C` 可优雅停止程序。

### 5.2 连接真实模块运行

1. 将 SIM 或兼容 eUICC 卡片插入模块。
2. 使用确认支持数据传输的 USB-C 线连接模块与 Mac。
3. 等待 macOS 完成 USB 设备枚举。
4. 启动程序：

```sh
./dist/djonehub-macos
```

程序会按以下顺序寻找管理通道：

1. 扫描 `/dev/cu.usbmodem*`、`/dev/cu.usbserial*` 和 `/dev/cu.wchusbserial*`，逐个探测可用 AT 串口。
2. 如果没有找到 AT 串口，则尝试通过 libusb 打开大疆 USB 设备 `2ca3:4006` 的 AT 接口。
3. 即使暂时未发现设备，HTTP 管理页面仍可能继续运行并等待设备重新连接。

如果自动识别选错串口，可先查看设备节点：

```sh
ls /dev/cu.*
```

然后显式指定 AT 串口：

```sh
./dist/djonehub-macos -port /dev/cu.usbmodemXXXX
```

### 5.3 启动参数

```text
-demo                 使用模拟数据运行，不访问硬件
-port <设备路径>      指定 AT 串口；省略时自动探测
-listen <地址:端口>   HTTP 监听地址，默认 127.0.0.1:7575
```

例如改用本机 8080 端口：

```sh
./dist/djonehub-macos -listen 127.0.0.1:8080
```

默认只监听回环地址，API 没有面向公网设计的认证层。除非已经配置额外的防火墙、反向代理和访问控制，否则不要监听 `0.0.0.0`，也不要把端口直接暴露到局域网或公网。

### 5.4 直接使用 `go run`

依赖自检通过后也可直接运行源码：

```sh
go run ./cmd/djonehub-macos -demo
go run ./cmd/djonehub-macos
```

`go run` 同样需要 CGO、`pkg-config` 和 libusb，并不会绕过原生依赖。

### 5.5 调试语音通话与 Voice Agent

源码模式需要两个长期运行的进程。API Key 必须设置在启动 Go 后端的同一个终端环境中；Swift 音频宿主不读取或持有云端密钥。

终端 1：

```sh
# 按实际使用的 Provider 设置一个或多个 Key
export DASHSCOPE_API_KEY='...'
export OPENAI_API_KEY='...'
export MINIMAX_API_KEY='...'

./dist/djonehub-macos
```

终端 2：

```sh
swift run --package-path macos/DJOneHubAudioHost \
  DJOneHubAudioHost --base-url http://127.0.0.1:7575
```

首次运行人工麦克风通话时，macOS 会请求麦克风权限。AI Agent 模式仍需要音频宿主连接模块 UAC，但上行媒体会转发给 Go 后端，模型合成的 8 kHz PCM 会由音频宿主写回模块。

可用组合：

| `provider` | 语音链路 | 必需密钥 |
| --- | --- | --- |
| `qwen` | Qwen Audio Realtime 原生双工 | `DASHSCOPE_API_KEY` |
| `openai` | OpenAI Realtime 原生双工 | `OPENAI_API_KEY` |
| `minimax` + `stt_provider=qwen` | Qwen STT → MiniMax LLM → MiniMax TTS | `DASHSCOPE_API_KEY`、`MINIMAX_API_KEY` |
| `minimax` + `stt_provider=openai` | OpenAI STT → MiniMax LLM → MiniMax TTS | `OPENAI_API_KEY`、`MINIMAX_API_KEY` |

常用模型和 Endpoint 覆盖：

```sh
# Qwen
export QWEN_REALTIME_MODEL=qwen-audio-3.0-realtime-flash
export QWEN_STT_MODEL=qwen3-asr-flash-realtime
export QWEN_REALTIME_ENDPOINT=wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime
export QWEN_STT_ENDPOINT=wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime

# OpenAI
export OPENAI_REALTIME_MODEL=gpt-realtime-2.1-mini
export OPENAI_STT_MODEL=gpt-live-transcribe
export OPENAI_STT_DELAY=low
export OPENAI_REALTIME_ENDPOINT=wss://api.openai.com/v1/realtime
export OPENAI_STT_ENDPOINT=wss://api.openai.com/v1/realtime

# MiniMax LLM/TTS
export MINIMAX_TEXT_MODEL=MiniMax-M2.7-highspeed
export MINIMAX_TTS_MODEL=speech-2.8-turbo
export MINIMAX_TTS_VOICE=male-qn-qingse
export MINIMAX_TEXT_ENDPOINT=https://api.minimax.io/v1/chat/completions
export MINIMAX_TTS_ENDPOINT=wss://api.minimax.io/ws/v1/t2a_v2
```

还可使用 `DJONEHUB_VOICE_AGENT_ENABLED`、`DJONEHUB_VOICE_AGENT_PROVIDER`、`DJONEHUB_VOICE_AGENT_FALLBACK_PROVIDER`、`DJONEHUB_VOICE_AGENT_VOICE`、`DJONEHUB_VOICE_AGENT_INSTRUCTIONS`、`DJONEHUB_VOICE_AGENT_STT_PROVIDER`、`DJONEHUB_VOICE_AGENT_FALLBACK_STT_PROVIDER`、`DJONEHUB_VOICE_AGENT_STT_MODEL`、`DJONEHUB_VOICE_AGENT_TOOLS_ENABLED`、`DJONEHUB_VOICE_AGENT_AUDIT_ENABLED`、`DJONEHUB_VOICE_AGENT_AUTO_ANSWER` 和 `DJONEHUB_VOICE_AGENT_AUTO_ANSWER_DELAY_MS` 设置首次启动默认值。若已有持久化 Profile，Profile 优先；API Key 仍只读取进程环境。

先检查密钥和 Provider 就绪状态：

```sh
curl http://127.0.0.1:7575/api/voice-agent/status
```

配置接口采用完整 Profile 替换语义。执行 `PUT` 时，请一并提交需要保留的模型、音色、提示词、STT 和自动接听字段。

启用 MiniMax，并选择 Qwen STT：

```sh
curl -X PUT http://127.0.0.1:7575/api/voice-agent/config \
  -H 'Content-Type: application/json' \
  -d '{
    "enabled": true,
    "provider": "minimax",
    "model": "MiniMax-M2.7-highspeed",
    "voice": "male-qn-qingse",
    "stt_provider": "qwen",
    "stt_model": "qwen3-asr-flash-realtime",
    "instructions": "你是电话客服，请用简洁自然的中文回答。",
    "auto_answer": false,
    "auto_answer_delay_ms": 1200
  }'
```

将 `stt_provider` 改为 `openai`、`stt_model` 改为 `gpt-live-transcribe`，即可切换 MiniMax 的转写阶段。启用 Qwen/OpenAI 原生双工和回退人工模式的完整请求见 [`docs/voice-agent-first-batch.md`](docs/voice-agent-first-batch.md)。

`auto_answer` 默认是 `false`。启用后，延迟范围会限制在 250–30000 ms，延迟结束时还会重新确认原来电仍为 `incoming/waiting`，避免对已经人工处理的电话重复发送 `ATA`。

同样的配置可以直接在管理页面“来电 → AI Voice Agent”完成。页面还提供实时转写、Provider 健康状态、工具确认和审计导出/清理。转写审计默认不持久化；启用后使用 `0600` JSONL 文件并默认遮盖电话、邮箱和长标识符。

检测到来电或外呼状态后，AI Agent 会在电话接通前预先建立 Provider WebSocket；媒体链路就绪后会立即主动、简短地问候对方，不再额外等待 3 秒。未接、拒接、换来电或修改 AI Profile 时会回收预连接。该策略同时适用于 Qwen/OpenAI Realtime 和 MiniMax 级联模式。请勿混用不同厂商的音色名称：OpenAI 推荐 `marin`/`cedar`，Qwen 默认 `Cherry`；管理页面会在切换 Provider 时自动修正。

Qwen/OpenAI Realtime 的输出 PCM 为 24 kHz，DJOneHub 会经过抗混叠滤波转换为模块电话链路使用的 8 kHz PCM；Qwen 会话的音频格式字段使用其协议规定的 `pcm`。音频宿主必须重新编译并和 Go 后端一起重启，才能应用抖动缓冲、长回复排队与 UAC 流控修复。中文识别默认传入 `zh` 和中文电话上下文，并使用 `gpt-4o-transcribe`。

长回复优化同时位于 Go 后端和 Swift 音频宿主：Go 使用跨 audio delta 连续的流式 FIR，并跨上行媒体消息保持插值状态；Swift 使用约 200 ms 首包抖动缓冲、欠载重新蓄水和零大块重排的读指针 PCM 队列，并根据 UAC 实际接收帧数推进。Go 通过独立 `audio.done` 事件释放尾包，不再把 usage 时序当作播放完成。升级时必须同时替换两端二进制。

## 6. 开发运行时的数据与日志

直接运行二进制时，程序日志默认输出到当前终端。eSIM Profile 的本地备注保存在 macOS 用户配置目录下，通常为：

```text
~/Library/Application Support/DJOneHub/profile-notes.json
```

Voice Agent Profile 不包含 API Key，文件权限为 `0600`：

```text
~/Library/Application Support/DJOneHub/voice-agent.json
~/Library/Application Support/DJOneHub/devices/<device-id>/voice-agent.json
```

使用发行包的 `djonehub` 启动器时，相关路径为：

```text
~/Library/Logs/DJOneHub/djonehub.log
~/Library/Application Support/DJOneHub/djonehub.pid
```

## 7. 构建 Apple Silicon 发行包

发行脚本与日常开发构建的区别是：它会下载并校验官方 libusb 1.0.30 源码，为 macOS 13/arm64 单独编译动态库，将动态库与主程序一起打包，并进行 ad-hoc 签名和动态依赖检查。

除第 2 节的工具外，打包过程还需要可用的 `curl`、`tar`、`shasum`、`codesign`、`otool` 和 `ditto`。这些工具大多由 macOS 或 Xcode Command Line Tools 提供。构建机必须能够访问 libusb 的 GitHub Release 下载地址。

执行：

```sh
./scripts/package-macos-arm64.sh v0.1.0-preview
```

如果不传版本参数，版本名默认为 `dev`：

```sh
./scripts/package-macos-arm64.sh
```

输出位于：

```text
dist/release/DJOneHub-macOS-arm64-v0.1.0-preview/
dist/release/DJOneHub-macOS-arm64-v0.1.0-preview.zip
dist/release/DJOneHub-macOS-arm64-v0.1.0-preview.zip.sha256
```

脚本会检查最终主程序是否仍引用 `/opt/homebrew`、`/usr/local` 或 Homebrew Cellar 路径；检查失败时不会把该构建视为可分发发行包。

打包完成后建议执行以下验收：

```sh
(
  cd dist/release
  shasum -a 256 -c DJOneHub-macOS-arm64-v0.1.0-preview.zip.sha256
)
otool -L dist/release/DJOneHub-macOS-arm64-v0.1.0-preview/bin/djonehub-macos
codesign --verify --verbose dist/release/DJOneHub-macOS-arm64-v0.1.0-preview/bin/djonehub-macos
codesign --verify --verbose dist/release/DJOneHub-macOS-arm64-v0.1.0-preview/bin/djonehub-audio-host
./dist/release/DJOneHub-macOS-arm64-v0.1.0-preview/djonehub start --demo
```

最后一条命令会保持前台运行并自动打开网页；完成验收后按 `Control+C` 停止。

## 8. 运行和安装发行包

### 8.1 免安装运行

不要拆散发行目录中的 `bin/` 和 `lib/`，在完整目录中执行：

```sh
./djonehub start
./djonehub start --demo
```

常用启动器命令：

```text
./djonehub start          启动并自动打开网页
./djonehub start --demo   启动无硬件演示模式
./djonehub stop           停止程序
./djonehub status         查看运行状态
./djonehub logs           跟踪日志，Control+C 退出
./djonehub open           重新打开管理网页
```

### 8.2 安装到系统路径

在完整发行目录中执行：

```sh
./install
```

安装器默认使用 `sudo`，安装位置为：

```text
/usr/local/libexec/djonehub
/usr/local/bin/djonehub
```

安装完成后可在任意目录执行：

```sh
djonehub start
djonehub status
djonehub logs
djonehub stop
```

## 9. 常见编译问题

### 9.1 Go 版本不满足要求

典型信息：

```text
go: go.mod requires go >= 1.26.3
```

或 Go 尝试下载 `go1.26.3` 工具链时因 DNS、代理、网络或缓存目录权限失败。请安装满足要求的 Go 版本，或确保 Go 工具链下载网络可用且 `GOPATH`/模块缓存可写。

### 9.2 找不到 `pkg-config`

典型信息：

```text
exec: "pkg-config": executable file not found in $PATH
```

解决：

```sh
brew install pkg-config libusb
pkg-config --modversion libusb-1.0
```

### 9.3 找不到 libusb

典型信息：

```text
Package libusb-1.0 was not found in the pkg-config search path
```

解决：

```sh
brew install libusb
export PKG_CONFIG_PATH="$(brew --prefix libusb)/lib/pkgconfig:${PKG_CONFIG_PATH:-}"
pkg-config --cflags --libs libusb-1.0
```

### 9.4 `CGO_ENABLED=0` 构建失败

这是当前项目的预期限制。USB AT 实现位于 `darwin && cgo` 构建条件下，主程序又会使用其中的接口与 AT 响应辅助函数，因此必须执行启用 CGO 的 macOS 构建。

### 9.5 运行时提示找不到 libusb 动态库

先查看引用路径：

```sh
otool -L ./dist/djonehub-macos
```

日常开发二进制依赖本机 Homebrew libusb，请确认它仍已安装。对外分发时不要复制这个开发二进制，应使用 `package-macos-arm64.sh` 生成自带动态库的完整发行目录。

### 9.6 端口 7575 已被占用

查看占用者：

```sh
lsof -nP -iTCP:7575 -sTCP:LISTEN
```

停止冲突进程，或改用其他本机端口：

```sh
./dist/djonehub-macos -listen 127.0.0.1:8080
```

### 9.7 找不到模块或 AT 串口

- 确认 USB-C 线支持数据传输，而不仅是充电。
- 执行 `system_profiler SPUSBDataType` 检查 macOS 是否看到 USB 设备。
- 执行 `ls /dev/cu.*` 检查串口节点。
- 大疆第一代模块的目标 USB 标识通常为 `2ca3:4006`。
- 可以用 `-port /dev/cu.xxx` 显式指定已确认的 AT 串口。
- 模式切换会导致 USB 重新枚举，短暂断开通常不是构建或程序故障。

### 9.8 macOS 阻止发行包运行

当前打包脚本使用 ad-hoc 签名，不是 Apple Developer ID 公证签名。首次运行可能需要到“系统设置 → 隐私与安全性”选择仍要打开。

仅在确认发行包来源可信并核对 SHA-256 后，才可在发行目录执行：

```sh
xattr -dr com.apple.quarantine ./djonehub ./bin ./lib
```

### 9.9 Voice Agent 显示 API Key 未配置

`/api/voice-agent/config` 返回 `API key is not configured` 时，通常是密钥没有进入 Go 后端进程的环境。请停止后端，在同一个终端先执行对应的 `export`，再重新启动。把 Key 只设置在 Swift 音频宿主的终端无效。

如果状态接口显示 Provider 已配置，但通话没有音频，请同时检查：

- `DJOneHubAudioHost` 是否正在运行；
- macOS 是否允许音频宿主使用麦克风；
- 通话页面的模块语音运行时和 D4/UAC Route 是否就绪；
- 当前是否只有一个 Agent 媒体会话；
- 后端日志中是否有 Provider WebSocket、采样率或模型名称错误。

## 10. 推荐的完整开发流程

```sh
# 1. 检查环境
go version
go env GOOS GOARCH CGO_ENABLED
pkg-config --modversion libusb-1.0

# 2. 获取依赖并测试
go mod download
go test -mod=mod ./...
swift build --package-path macos/DJOneHubAudioHost -c debug

# 3. 开发构建 Go 主程序
./scripts/build-macos.sh

# 4. 先进行无硬件验证（演示模式不启动音频宿主）
./dist/djonehub-macos -demo

# 5. 再连接真实模块验证；语音功能需在另一终端启动 Swift 音频宿主
./dist/djonehub-macos
swift run --package-path macos/DJOneHubAudioHost \
  DJOneHubAudioHost --base-url http://127.0.0.1:7575

# 6. 发布前制作自带 libusb 的发行包
./scripts/package-macos-arm64.sh v0.1.0-preview
```

涉及 eSIM Profile 下载、启用、改名和删除时，会真实修改实体 eUICC。真机调试期间不要拔出模块、切换 USB 模式或强制终止正在进行的卡片写入操作。
