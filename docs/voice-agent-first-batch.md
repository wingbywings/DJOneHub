# Voice Agent 第一批实施方案

## 范围与当前状态

第一批采用一个 Provider 无关的 Voice Runtime，同时支持两种引擎：

- 原生双工语音：Qwen Audio Realtime、OpenAI Realtime。
- 级联语音：可插拔 STT → MiniMax LLM → MiniMax TTS。

已落地的代码包括：统一会话/事件接口、Provider 注册表、8 kHz 电话 PCM 重采样、Qwen/OpenAI WebSocket 适配器、MiniMax 分句流式 LLM/TTS、本地 Go ↔ Swift WebSocket 媒体桥、人工/Agent 媒体模式切换、Web Profile、实时转写、脱敏审计和受控工具。

第二阶段已经补齐 Qwen 与 OpenAI 流式 STT。MiniMax 级联 Profile 可以通过 `stt_provider` 独立选择识别模型，且选择会持久化到每个设备自己的配置文件。

## 音频与事件约定

- Swift 与 Go 之间固定使用 PCM signed 16-bit little-endian、单声道、8 kHz。
- WebSocket 二进制帧为电话 PCM：Swift → Go 是来电方语音，Go → Swift 是 Agent 合成语音。
- WebSocket 文本帧是标准化状态/转写事件，硬件宿主忽略，业务层可用于日志与实时字幕。
- Provider 内部自行转换采样率：Qwen 双工模型输入 16 kHz、输出 24 kHz；OpenAI 双工模型输入/输出 24 kHz；Qwen STT 直接使用电话原生 8 kHz；OpenAI STT 使用 24 kHz；MiniMax TTS 直接请求 8 kHz PCM。

## 配置

密钥只由 Go 后端读取，不下发到 Swift 或浏览器。

```bash
# Qwen
export DASHSCOPE_API_KEY=...
export QWEN_REALTIME_MODEL=qwen-audio-3.0-realtime-flash
export QWEN_STT_MODEL=qwen3-asr-flash-realtime

# OpenAI
export OPENAI_API_KEY=...
export OPENAI_REALTIME_MODEL=gpt-realtime-2.1-mini
export OPENAI_STT_MODEL=gpt-live-transcribe
export OPENAI_STT_DELAY=low

# MiniMax 级联的 LLM/TTS 阶段
export MINIMAX_API_KEY=...
export MINIMAX_TEXT_MODEL=MiniMax-M2.7-highspeed
export MINIMAX_TTS_MODEL=speech-2.8-turbo
export MINIMAX_TTS_VOICE=male-qn-qingse
```

也可以用以下变量设置首次启动默认 Profile：

```bash
export DJONEHUB_VOICE_AGENT_ENABLED=true
export DJONEHUB_VOICE_AGENT_PROVIDER=qwen
export DJONEHUB_VOICE_AGENT_VOICE=Cherry
export DJONEHUB_VOICE_AGENT_INSTRUCTIONS='你是电话客服，请用简洁自然的中文回答。'
export DJONEHUB_VOICE_AGENT_STT_PROVIDER=qwen
export DJONEHUB_VOICE_AGENT_STT_MODEL=qwen3-asr-flash-realtime
export DJONEHUB_VOICE_AGENT_FALLBACK_PROVIDER=openai
export DJONEHUB_VOICE_AGENT_FALLBACK_STT_PROVIDER=openai
export DJONEHUB_VOICE_AGENT_TOOLS_ENABLED=false
export DJONEHUB_VOICE_AGENT_AUDIT_ENABLED=false
export DJONEHUB_VOICE_AGENT_AUTO_ANSWER=false
export DJONEHUB_VOICE_AGENT_AUTO_ANSWER_DELAY_MS=1200
```

如果本机已经存在持久化 Profile，Profile 会覆盖这些首次启动默认值。API Key 始终只从进程环境读取，不会从 Profile 加载。

所有 Endpoint 均可覆盖，便于切换国际站、中国站、代理或测试服务器：

```bash
export QWEN_REALTIME_ENDPOINT=wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime
export QWEN_STT_ENDPOINT=wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime
export OPENAI_REALTIME_ENDPOINT=wss://api.openai.com/v1/realtime
export OPENAI_STT_ENDPOINT=wss://api.openai.com/v1/realtime
export MINIMAX_TEXT_ENDPOINT=https://api.minimax.io/v1/chat/completions
export MINIMAX_TTS_ENDPOINT=wss://api.minimax.io/ws/v1/t2a_v2
```

## 启用与回退

推荐直接在管理页面进入“来电 → AI Voice Agent”配置。页面包含 Provider/模型/音色、MiniMax STT、两级故障降级、角色指令、自动接听、受控工具、审计和脱敏，并显示实时转写、Provider 健康状态和待确认操作。以下 HTTP API 仍适合自动化和故障排查。

查询 Provider 是否就绪：

```bash
curl http://127.0.0.1:7575/api/voice-agent/status
```

`PUT /api/voice-agent/config` 会替换完整 Profile，而不是只修改提交的字段。更新配置时应同时提交需要保留的 `provider`、模型、音色、提示词和自动接听设置。

启用 Qwen：

```bash
curl -X PUT http://127.0.0.1:7575/api/voice-agent/config \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true,"provider":"qwen","voice":"Cherry","instructions":"你是电话客服，请用简洁自然的中文回答。"}'
```

启用 OpenAI：

```bash
curl -X PUT http://127.0.0.1:7575/api/voice-agent/config \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true,"provider":"openai","voice":"marin","instructions":"你是电话客服，请用简洁自然的中文回答。"}'
```

启用 MiniMax，并使用 Qwen STT：

```bash
curl -X PUT http://127.0.0.1:7575/api/voice-agent/config \
  -H 'Content-Type: application/json' \
  -d '{
    "enabled": true,
    "provider": "minimax",
    "fallback_provider": "openai",
    "model": "MiniMax-M2.7-highspeed",
    "voice": "male-qn-qingse",
    "stt_provider": "qwen",
    "fallback_stt_provider": "openai",
    "stt_model": "qwen3-asr-flash-realtime",
    "instructions": "你是电话客服，请用简洁自然的中文回答。",
    "tools_enabled": true,
    "audit_enabled": true,
    "redact_pii": true,
    "auto_answer": true,
    "auto_answer_delay_ms": 1200
  }'
```

把 `stt_provider` 改为 `openai`、`stt_model` 改为 `gpt-live-transcribe`，即可让 MiniMax 使用 OpenAI STT。

随时回退人工麦克风：

```bash
curl -X PUT http://127.0.0.1:7575/api/voice-agent/config \
  -H 'Content-Type: application/json' \
  -d '{"enabled":false,"provider":"qwen"}'
```

切换只改变媒体源，不改动现有接听、拨号、挂断、DTMF 和 UAC 路由逻辑。已接通通话会由 Swift 宿主在下一次 500 ms 配置轮询时重建媒体通道。

Profile 不包含 API Key。单设备模式保存于 `~/Library/Application Support/DJOneHub/voice-agent.json`；多设备模式保存于 `~/Library/Application Support/DJOneHub/devices/<device-id>/voice-agent.json`。文件权限为 `0600`，更新采用临时文件加原子重命名。

自动接听默认关闭。启用后，服务会等待 `auto_answer_delay_ms`，然后再次确认原通话仍处于 `incoming/waiting` 状态才发送 `ATA`；若来电已被人工接听、拒接或挂断，不会继续执行。

## Web、工具与审计

- `GET /api/voice-agent/events` 使用 SSE 推送标准化转写、VAD、错误、工具和会话状态。
- `GET /api/voice-agent/audit` 读取最近记录；`DELETE` 必须提交 `{"confirm":true}`。
- 审计文件为 `voice-agent-audit.jsonl`，权限 `0600`，达到 5 MiB 自动轮转；内存和文件各最多保留有界数据。
- 审计不保存 PCM。录音仍保存在 recordings 目录，审计只记录录音开始、停止和路径元数据。
- 电话状态查询和挂断会直接执行；Voice Agent 的 DTMF 工具暂时停用，网页人工通话键盘不受影响。
- MiniMax 工具多轮保留完整 assistant `tool_calls` 与 `tool_call_id` 历史，确认后可以继续生成和播报结果。

## 可靠性与延迟

- Swift 媒体桥在断线后按 1、2、4、8 秒指数退避重连；切换 Profile revision 会重建活动媒体会话。
- Provider/STT 失败会进入最长 32 秒的熔断窗口；显式配置降级项后，下一次媒体重连优先选择健康候选。
- Qwen Realtime 使用可过滤无语义背景声的 `smart_turn`；OpenAI Realtime 和级联 STT 使用更适合电话噪声的较高 VAD 阈值、近场降噪与更长静音确认。确认 `speech.started` 后，音频宿主立即清空未播放的 Agent PCM。
- 首次轮询到来电或外呼状态时即预连 Provider WebSocket；电话媒体 WebSocket 就绪后立即发送一次主动问候，不再使用 3 秒静默计时。未接、拒接、换来电或 Profile revision 变更时会取消并关闭预连会话，避免泄漏空闲连接。
- 音色按 Provider 归一化：OpenAI 内置音色使用 `marin`、`cedar` 等官方名称，Qwen 默认使用 `Cherry`；跨 Provider 降级不会沿用不兼容的音色。
- Qwen/OpenAI 的 24 kHz PCM 在进入 8 kHz 电话链路前执行抗混叠低通，Qwen 会话使用其协议规定的 `pcm` 格式名；Swift 宿主只移除 UAC 实际接收的帧，被环形缓冲拒绝的帧保留在队列。Agent 音频队列安全上限为 300 秒，避免云端生成速度快于电话播放时钟时截断回复。
- 长回复使用跨 audio delta 保留 FIR 历史和抽取相位的流式降采样器，上行 8→16 kHz 插值也跨媒体消息保持连续。Swift 队列通过读指针消费，在首包播放前保留约 200 ms 抖动缓冲，并在云端分片短暂欠载时利用 UAC 环形缓冲重新蓄水；播放尾包由 `audio.done` 明确放行，不再错误依赖提前到达的 usage。UAC 暂时满载时不再复制整段 `Data`。正常电话策略仍要求单轮约 20 秒并对复杂内容分段。
- 中文通话使用 `language: zh`、中文转写提示、`gpt-4o-transcribe`、近场降噪和较低的电话 VAD 阈值；输入转写用于界面与审计，Realtime 模型仍直接消费原始音频。
- MiniMax 文本响应使用 SSE；完整句子一到达就开始 TTS，不再等待整段 LLM 响应完成。开启工具时，为保证工具调用完整性，当前轮回退到非流式文本完成。

## 剩余验收边界

软件侧剩余里程碑已经实现并有自动化测试覆盖。发布前仍需使用真实模块、SIM、运营商 VoLTE 和有效云端 Key 完成呼入/呼出、弱网、长通话、连续打断、跨 Provider 计费与多模块拔插验收；这些结果依赖外部硬件和账号，不能由离线测试替代。
