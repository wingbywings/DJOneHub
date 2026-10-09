# 远程接听：第六批受认证的本机宿主控制

日期：2026-10-01。分支：`codex/remote-iphone-callkit`。

本批把第五批的可信宿主就绪接口接到独立本机控制服务，并改造 Swift 宿主读取凭证、连接每通电话的媒体桥、上报 UAC 状态和清理音频。**公网配对/信令、APNs 和 iOS App 仍未实现，实际来电也尚未自动创建远程会话；状态接口继续返回不可用。** 本批验证不等于真实模块或 iPhone 已通过验收。

## 本机控制服务

Go 程序新增 `--native-control-file` 参数。启用时创建只监听 `127.0.0.1` 随机端口的 HTTP 服务；它与现有 Web 管理端口分开。

- 生成 256 bit 随机控制 token，通过原子写入的 0600 JSON 文件交付；父目录必须属于当前用户且权限不向组或其他用户开放。
- 启动凭证包括协议版本、loopback base URL 和 token。重启更换端口/token；同一路径受进程文件锁保护，第二个服务不能覆盖仍运行实例的凭证。正常关闭移除凭证文件，保留可复用的空锁文件。
- 请求同时检查真实 TCP peer 为 loopback、拒绝 Origin，忽略转发头；控制 token 放在 Authorization header 中。返回 `Cache-Control: no-store`。
- 每通电话沿用 HostBridge 自己的随机媒体 token。控制 token 不能用于媒体 WebSocket，媒体 token 也不能调用配置或状态接口。
- token 不放进命令行、URL、普通状态或日志。现有公开宿主配置只返回通话身份及模式，不返回远程媒体凭证。

该边界把本机同一用户账户中的宿主视为可信程序；不宣称能够隔离已经获得该账户文件读取权限的进程。

## 私有接口

这些路径只注册在独立 native listener，不在 Web 管理 listener：

| 接口 | 作用与约束 |
| --- | --- |
| `GET /config?device_id=...` | 控制 token + 宿主实例 ID；远程 owner、蜂窝 active、会话未结束且通话身份匹配时，才返回本通媒体 URL/token |
| `POST /status?device_id=...` | 控制 token + 同一宿主实例 ID；上报 call UUID、generation、递增 sequence、running |
| `GET /media/{deviceID}/{callUUID}` | WebSocket；只接受本通媒体 token，继续检查当前 owner 和单次桥连接 |

首次获取远程配置的宿主实例占有本代会话，另一个实例不能获取其媒体凭证或伪造就绪。切换宿主必须重建媒体 generation。

多模块模式要求明确的 device ID；模块消失时拒绝请求，不改为操作默认模块。单模块服务使用空 device ID，媒体路径内部用 `_single` 表示。

## 就绪与终止

`startRemoteSession` 的媒体工厂返回带 `Host()` 的真实 Peer 时，自动建立对应 native host 状态；不再需要外部传入“始终就绪”的回调。宿主就绪必须同时满足：

1. 当前 call UUID / media generation / 宿主实例匹配。
2. Swift 已完成实际 UAC 启动，并完成对应 HostBridge ready 握手。
3. 控制服务收到递增的 running 上报，距当前时间不足 2 秒。
4. 本通 HostBridge 仍连接。

过期、重复或错代上报不能延长就绪。旧公开 `audio/host/register` 不参与这一判定。可信状态也用于更新现有 UI 的宿主运行状态。

Swift 在约 500 ms 的控制轮询中报告状态；远程模式不等待逐模块的旧状态上报，以免旧请求拖延心跳。控制请求预算为 1 秒，资源请求上限 2 秒。UAC 错误、媒体连接错误或控制失败时停止音频、清空队列并尝试发送 running=false；已经运行的宿主报告停止时，后端立即关闭对应媒体桥。会话结束会关闭 WebSocket，宿主据此停止 UAC；失联还受就绪过期及媒体输入超时约束。

这些预算是初始策略，不是已经测得的硬实时上限，仍需在多模块与真机负载下验证。

## Swift 凭证处理

新增可测试的 `RemoteAudio.NativeControlClient`：

- 每次控制请求重新读取启动文件，支持服务重启后的轮换。
- 使用 `O_NOFOLLOW` 打开文件，检查为当前用户拥有的私有普通文件，限制大小；拒绝符号链接、公开可读文件和不合法凭证。
- 仅接受 `http://127.0.0.1:<port>` 的 bootstrap，不接受域名、远端地址、用户信息或带查询参数的 base URL。
- 控制和媒体客户端禁用 HTTP 重定向及代理设置；媒体相对路径不得跳出 native origin。
- 使用进程级随机宿主 ID；状态序号与 call UUID/generation 一起阻止迟到的停止或启动上报影响下一通电话。
- 远程凭证缺失或失效时停止远程音频，不退回 Mac 麦克风/扬声器。AI/本地媒体继续使用原有路径。

## 开发启动

这是显式启用的开发入口，当前发行启动器尚未自动传参。分别在两个终端启动后端与宿主，使用同一个绝对文件路径，例如：

```sh
mkdir -p "$HOME/Library/Application Support/DJOneHub/native-control"
chmod 700 "$HOME/Library/Application Support/DJOneHub/native-control"

./djonehub-macos --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

```sh
./djonehub-audio-host --base-url http://127.0.0.1:7575 \
  --native-control-file "$HOME/Library/Application Support/DJOneHub/native-control/bootstrap.json"
```

这只开启本机宿主控制，不会启用远程来电邀请。端到端远程能力仍需要后续公网信令创建已授权会话，以及带 `remoteopus` 的后端构建。

## 验证

新增 4 项 Go 测试覆盖鉴权/来源边界、公开接口无 token、宿主实例占用、实际 WebSocket 握手后的就绪确认、陈旧心跳、停止/终态、文件权限、进程锁、重启轮换及路径保护。新增 4 项 Swift 测试覆盖文件权限/符号链接、凭证轮换、非 loopback URL、媒体路径限制；Swift 测试总计 8 项。

已通过全量 Go 测试、真实 WebRTC/Opus 与协调层竞态测试，以及 Swift 宿主编译和全部 8 项 Swift 测试。Go 本机控制测试实际监听 TCP 并建立 WebSocket；模拟 UAC/蜂窝状态用于验证协调逻辑，没有操作真实电话或音频硬件。

```sh
go test ./...
go test -race -tags remoteopus ./internal/remotecall ./internal/remotemedia/... ./internal/callcontrol ./cmd/djonehub-macos -timeout 90s
swift test --package-path macos/DJOneHubAudioHost
```

配对、安装身份与凭证撤销已继续实现，见 [第七批配对与安装身份](remote-calls-seventh-batch.md)。后续通过公网信令将内部远程会话接入实际来电，再接 APNs/TURN 与 iOS CallKit App。既有媒体自适应、时钟补偿、网络恢复和发布验证仍待完成。
