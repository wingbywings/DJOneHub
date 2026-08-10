# DJOneHub 中文编译与运行指南

本文面向需要从源码编译、调试或制作 DJOneHub macOS 发行包的开发者。普通用户如只想使用软件，建议直接下载项目 Release 中的 `macOS-arm64` ZIP，并参阅根目录的 `README.md`。

## 1. 项目与构建方式概览

DJOneHub 是一个面向大疆第一代 4G 模块的 macOS 本地管理程序，主要技术组成如下：

| 目录/文件 | 作用 |
| --- | --- |
| `cmd/djonehub-macos/main.go` | macOS 主程序入口、HTTP API、设备状态、短信、eSIM、网络与流量功能 |
| `cmd/djonehub-macos/usbat_darwin.go` | 通过 CGO 和 libusb 访问大疆模块 USB AT 接口 |
| `cmd/djonehub-macos/web/` | 使用 `go:embed` 编译进二进制的原生管理页面 |
| `internal/` | APDU 仲裁、设备后端、配置、eSIM、调制解调器和 SIM AID 等内部实现 |
| `pkg/` | 日志、MBIM、短信 PDU 编解码等共享包 |
| `third_party/` | 由 `go.mod` 的 `replace` 指令引用的本地第三方源码 |
| `scripts/build-macos.sh` | 依赖本机 libusb 的日常开发构建脚本 |
| `scripts/package-macos-arm64.sh` | 构建并打包自带 libusb 的 Apple Silicon 发行包 |
| `packaging/` | 发行包启动器、安装器和发行说明 |

项目没有需要单独构建的 Vue、React 或 Node.js 前端。`cmd/djonehub-macos/web/` 中的 HTML、CSS 和 JavaScript 会随 Go 程序一起嵌入二进制。

需要特别注意：

- 根模块当前仍名为 `github.com/iniwex5/vohive`，这是现有源码的真实导入路径，不影响 DJOneHub 构建，不要仅为编译而修改它。
- `go.mod` 声明的 Go 版本为 **1.26.3**。
- macOS 主程序依赖 CGO 和 libusb，不能用 `CGO_ENABLED=0` 代替正常构建。
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
pkg-config --modversion libusb-1.0
```

在 Apple Silicon Mac 上，关键结果应满足：

- `GOOS` 为 `darwin`
- `GOARCH` 为 `arm64`
- `CGO_ENABLED` 为 `1`
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
```

测试 `cmd/djonehub-macos` 时同样会编译 libusb/CGO 代码，因此缺少 `pkg-config` 或 libusb 会导致该包构建失败。不要使用 `CGO_ENABLED=0` 绕过：当前无 CGO 替代文件只用于有限的平台占位，并不足以构建完整主程序。

需要查看具体测试过程时，可使用：

```sh
go test -mod=mod -v ./cmd/djonehub-macos
go test -mod=mod -v ./internal/...
go test -mod=mod -v ./pkg/...
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

## 6. 开发运行时的数据与日志

直接运行二进制时，程序日志默认输出到当前终端。eSIM Profile 的本地备注保存在 macOS 用户配置目录下，通常为：

```text
~/Library/Application Support/DJOneHub/profile-notes.json
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

## 10. 推荐的完整开发流程

```sh
# 1. 检查环境
go version
go env GOOS GOARCH CGO_ENABLED
pkg-config --modversion libusb-1.0

# 2. 获取依赖并测试
go mod download
go test -mod=mod ./...

# 3. 开发构建
./scripts/build-macos.sh

# 4. 先进行无硬件验证
./dist/djonehub-macos -demo

# 5. 再连接真实模块验证
./dist/djonehub-macos

# 6. 发布前制作自带 libusb 的发行包
./scripts/package-macos-arm64.sh v0.1.0-preview
```

涉及 eSIM Profile 下载、启用、改名和删除时，会真实修改实体 eUICC。真机调试期间不要拔出模块、切换 USB 模式或强制终止正在进行的卡片写入操作。
