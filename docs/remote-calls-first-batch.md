# 远程接听：第一批后端基础

对应 [完整落地方案](REMOTE_IPHONE_CALLKIT_IMPLEMENTATION_PLAN.md) 中 P1 的路由与配置基础。此批尚未完成整个 P1，也未实现远程电话接听。

## 已实现

- 每个模块独立保存远程配置，原子替换文件并使用 `0600` 权限；文件无效时关闭远程配置并报告错误。
- 新观察到的电话具有 `call_uuid`，保留现有 `id`，状态和历史记录均包含固定的 `route` 快照。
- AI 优先、远程其次、本地最后。AI 与远程没有失败回退关系；远程配置不会打开 AI。
- AI 预连接、自动接听、AI 录音及媒体接口检查当前通话路由，远程来电不能进入 AI 链路。
- 来电创建与接听方式配置更新串行处理；通话期间切换 AI 开关、修改远程配置返回 `409 route_locked_by_call`。相同远程配置的重试仍可成功。
- 现有本地外呼不会因为启用远程接听被分配到远程路由。

`route` 表示选定的处理分支，并非已接听者或媒体已经连通。公网及手机端尚未实现，因此 `available` 始终为 false，`signaling`、`voip_push`、`remote_media` 能力均为 false。此时开启配置仅保存接听意图，仍可使用已有本地接听。

## API

单模块兼容前缀为 `/api/remote-calls`。多模块使用 `/api/devices/{deviceID}/remote-calls`，需填入真实模块 ID。

| 方法及路径 | 用途 |
| --- | --- |
| `GET /config` | 读取配置 |
| `PUT /config` | 完整替换配置；revision 由服务端分配 |
| `GET /status` | 读取配置、选定路由、当前通话路由、可用性与错误原因 |

以下是开发期配置示例。`phone-example` 只是格式合法的占位标识；此批还没有配对服务，不能验证它是否真实已配对。后续开放远程接听前必须加入配对授权校验。

```sh
curl -X PUT http://127.0.0.1:7575/api/remote-calls/config \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true,"target_installation_id":"phone-example","ring_timeout_seconds":45,"caller_display":"number"}'

curl http://127.0.0.1:7575/api/remote-calls/status
```

配置字段：

- `enabled`：远程接听意图开关，默认 false。
- `target_installation_id`：开启时必填，最多 128 个 ASCII 字母、数字、短横线或下划线。
- `ring_timeout_seconds`：默认 45，范围 10–90；当前只是持久化及快照字段，振铃截止调度尚未接入。
- `caller_display`：`number` 或 `module`，默认 `number`。
- `revision`：服务端版本号，客户端不能指定最终值。

`reason` 可能为 `disabled`、`configuration_error`、`ai_route_selected`、`transport_not_implemented`。AI 启用时即使 Provider 出错，仍显示 `ai_route_selected`，不会转为远程分支。

单模块配置默认保存于 `~/Library/Application Support/DJOneHub/remote-calls.json`；多模块保存于 `devices/<deviceID>/remote-calls.json`。演示模式默认只保存在内存，测试可指定临时路径。

## 验证及下一批

新增测试覆盖路由组合、UUID 生命周期、配置落盘失败、损坏文件恢复、模块隔离、配置与来电并发、远程电话被旧 AI 状态误处理的防护，以及 AI 开关在通话中的限制。

```sh
go test ./...
go test -race ./internal/callcontrol ./cmd/djonehub-macos
```

后续进展见 [第二批通话命令与资源仲裁](remote-calls-second-batch.md)：已接入本地/AI 的 UUID 绑定命令、幂等、接听前全局租约及迟到操作隔离，文档同时列出了尚未完成的取消与控制边界。之后接入独立 Swift remote 媒体模式与 Opus/WebRTC；iOS App、公网配对、APNs 和 TURN 按完整方案实施。

P0 中依赖 Apple 签名、APNs、真实 iPhone 和模块的验证仍未执行。此批通过自动化测试不代表远程接听已经可用。
