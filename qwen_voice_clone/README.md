# Qwen-Audio 声音复刻工具

本地 Web 工具：上传 WAV / MP3 / M4A 音频，先写入百炼 48 小时临时 OSS，随后调用
Qwen-Audio 声音复刻接口，并将返回的 `voice_id` 持久化到 SQLite。默认目标模型为
`qwen-audio-3.0-realtime-plus`，适合直接把生成结果填入实时模型的 `voice` 参数。

## 运行

```bash
python3 -m venv .venv
source .venv/bin/activate
pip install -r qwen_voice_clone/requirements.txt

export DASHSCOPE_API_KEY='sk-...'
export DASHSCOPE_WORKSPACE_ID='你的百炼业务空间 ID'
python3 -m qwen_voice_clone.app
```

服务默认监听所有 IPv4 网卡的 `7576` 端口。本机打开 <http://127.0.0.1:7576>；
局域网或其他可路由设备使用 `http://服务器IP:7576` 访问。

可通过环境变量覆盖监听地址和端口，例如：

```bash
HOST=192.168.1.10 PORT=9000 python3 -m qwen_voice_clone.app
```

API Key 只从服务端环境变量读取，不会返回给浏览器或写入数据库。成功记录默认保存在
`qwen_voice_clone/data/voices.sqlite3`，可用 `VOICE_DB_PATH` 修改路径。

## 接口

- `POST /api/voices`：`multipart/form-data`，字段为 `audio`、`prefix`、
  `target_model`、`language`、`enable_preprocess`。
- `GET /api/voices`：读取已保存的音色 ID 历史。

## 注意

- 本工具按 Qwen-Audio-Realtime 的华北 2（北京）流程实现；API Key 与 Workspace ID
  必须属于该地域。
- 上传音频应为 10–20 秒（最长 60 秒）、不超过 10 MB、采样率不低于 16 kHz，并包含
  至少 5 秒无背景音的连续清晰朗读。
- 临时 OSS URL 有效期为 48 小时，只适用于开发和低并发使用；生产环境应改用自有 OSS。
- `target_model` 必须与之后真正调用的 Qwen-Audio 模型完全一致。
