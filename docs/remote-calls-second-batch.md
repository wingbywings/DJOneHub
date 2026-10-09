# 远程接听：第二批通话命令与资源仲裁

本批继续在 `codex/remote-iphone-callkit` 分支实现 P1，接入现有网页和 AI 的接听、拒接、挂断与 DTMF。远程 App、信令、VoIP Push 和 Opus/WebRTC 尚未实现；远程状态接口仍返回 `available: false`。

## 已实现

- `internal/callcontrol.Commands` 统一去重：同一 `request_id`、同一完整命令等待或返回原结果；复用 ID 修改命令返回冲突。成功与失败都保留 10 分钟，每模块最多 1024 条，不提前淘汰未过期记录。进程重启不保留命令缓存，新观察到的电话会分配新 UUID。
- `POST /api/calls/{callUUID}/commands` 以 UUID 和 `expected_revision` 绑定电话。网页旧入口也发送这些字段；历史客户端省略 UUID 时仅兼容为请求到达时的电话快照。新客户端应使用严格入口。
- 接听前先获取跨模块共享的音频租约，再准备模块语音，最后复核并发送 ATA。资源忙时返回 `409 audio_busy`，不发送接听命令。租约包含通话 UUID 和递增 generation，旧租约不能释放新租约。
- 接听准备及 AT 锁等待受 12 秒命令预算、请求取消和通话生命周期控制。挂断、来电结束、替换或 USB 拔出会取消待接听任务。即使准备工作迟到返回，也不能提交旧接听；同一来电重试时，旧任务不能清理新任务的状态或资源。
- 真机命令在持有操作锁时重新读取 CLCC，核对唯一通话、索引、方向、号码和可执行状态，拒绝多路/等待通话的模糊控制。已观察到替换时生成新 UUID；外呼的临时索引 0 转为真实索引时保留 UUID。
- 已结束且仍在最近 100 条历史内的通话，重复挂断返回成功而不发送 AT。AI 延迟接听和告别语播放后的挂断均绑定原 UUID。
- ATA 明确 ERROR 时允许释放接听占用；超时或缺少 OK 属于结果未知，保留占用及 `answer_pending`，等待 CLCC 确认接通或结束。DTMF/挂断仅在明确 ERROR 后考虑兼容命令，超时不直接补发，避免重复按键或误操作。
- 模块预备 session 和路由启停携带通话 UUID；旧电话的路由清理不关闭新电话已归属的 session。手动停止媒体不释放仍在通话中的音频租约。
- AI 与远程仍按第一批固定路由规则互斥，未增加任何跨分支回退。

## 命令接口

单模块前缀 `/api/calls`；多模块前缀 `/api/devices/{deviceID}/calls`。先读取 `GET /status` 获取 `active.call_uuid` 和 `active.revision`。

```http
POST /api/calls/{call_uuid}/commands
Content-Type: application/json

{
  "request_id": "本次操作生成的唯一ID，网络重试保持不变",
  "expected_revision": 3,
  "type": "answer"
}
```

`type` 支持 `answer`、`reject`、`hangup`、`dtmf`。DTMF 另带 `digit`，仅允许 `0-9*#` 的单个字符。JSON 不能指定可信 actor，本批 HTTP 入口为本地控制，AI 走内部可信调用；未来远程入口必须先通过配对鉴权再接入。

成功响应示意：

```json
{"call_uuid":"...","type":"answer","accepted":true,"response":"ATA\r\nOK"}
```

`accepted` 表示命令获确认（或已结束通话的幂等挂断），不能当作媒体已连通。以状态接口中的 `state` 判断 CLCC 确认的电话状态。`control_state` 为 `answering`、`answer_pending`、`answered`、`ending_pending` 或省略，用于解释准备和待确认状态。

常见错误：

| HTTP | code | 客户端处理 |
| --- | --- | --- |
| 400 | `invalid_command` | 修正命令字段 |
| 409 | `stale_revision` | 刷新状态；确实需要新操作时使用新 request_id |
| 409 | `call_ended` / `call_changed` / `ambiguous_call` | 停止原操作，不改为操作当前电话 |
| 409 | `request_id_conflict` | 同一 ID 必须携带完全相同的命令 |
| 409 | `audio_busy` / `answer_in_progress` | 当前操作不能获得资源或该来电已有控制操作 |
| 408 | `command_cancelled` | 查询电话状态，不假设 ATA 未执行 |
| 429 | `command_capacity` | 稍后重试 |
| 502 | `modem_error` | 查询电话状态和模块错误；失败结果同样参与去重 |

## 验证

新增 15 个测试函数，覆盖并发去重、失败缓存、容量上限、过期清理、旧 generation、旧 revision、准备中换电话、挂断取消、两模块争用、CLCC 多路/号码变化、DTMF 超时、延迟 AI 挂断、同一电话新旧接听任务交错，以及明确拒绝与未知 ATA 结果的不同处理。

已通过：

```sh
go test ./...
go test -race ./internal/callcontrol ./cmd/djonehub-macos
node --check cmd/djonehub-macos/web/app.js
git diff --check
```

另用 Node VM 执行实际网页 `runCallAction` 函数，验证传递状态接口的 `call_uuid` / `revision`、切换模块后阻止旧操作，以及 HTTP 局域网环境中没有 `crypto.randomUUID` 时的随机 ID 回退。

## 尚待完成

1. 完整远程 answer-ready/ICE/音频激活状态机、事件订阅和授权 owner；本批提供本地/AI 控制基础，不能作为公网 API 直接发布。
2. 模块底层 ADB/USB 准备步骤的逐步取消。本批可立即取消等待并拦截迟到 ATA；不可中断的底层步骤仍会运行到其自身超时，再在锁内清理。
3. 外呼的接听前资源策略对等迁移，以及静音/录音/手动媒体控制的 UUID 绑定。当前外呼仍走原拨号流程，远程外呼不在首版范围。
4. CLCC 本身没有通话 UUID；两次轮询之间若电话被同索引、同方向、同号码、同状态的新电话替代，软件无法可靠识别。当前最终 CLCC 检查缩短竞态窗口，后续需结合 URC/更密集轮询及真机验证，不能宣称硬件级原子控制。
5. P2 独立远程媒体与 Opus/WebRTC，P3 iOS/CallKit、配对、APNs、TURN；P0 真机签名、后台唤醒及真实模块音频验证尚未完成。
