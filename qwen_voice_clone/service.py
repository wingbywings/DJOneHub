from __future__ import annotations

import re
import wave
from dataclasses import dataclass
from pathlib import Path
from typing import Any
from uuid import uuid4

import requests


ALLOWED_SUFFIXES = {".wav", ".mp3", ".m4a"}
MAX_AUDIO_BYTES = 10 * 1024 * 1024
PREFIX_PATTERN = re.compile(r"^[A-Za-z0-9]{1,10}$")
DEFAULT_TARGET_MODEL = "qwen-audio-3.0-realtime-plus"


class VoiceCloneError(RuntimeError):
    """A safe, user-facing error raised by the voice cloning workflow."""


@dataclass(frozen=True)
class CloneResult:
    voice_id: str
    request_id: str | None


def validate_audio(path: Path, original_name: str, size: int) -> None:
    suffix = Path(original_name).suffix.lower()
    if suffix not in ALLOWED_SUFFIXES:
        raise VoiceCloneError("仅支持 WAV、MP3 或 M4A 音频文件")
    if size <= 0:
        raise VoiceCloneError("音频文件不能为空")
    if size > MAX_AUDIO_BYTES:
        raise VoiceCloneError("音频文件不能超过 10 MB")
    if not path.is_file():
        raise VoiceCloneError("没有找到上传的音频文件")

    try:
        if suffix == ".wav":
            with wave.open(str(path), "rb") as audio:
                sample_rate = audio.getframerate()
                channels = audio.getnchannels()
                sample_width = audio.getsampwidth()
                duration = audio.getnframes() / sample_rate if sample_rate else 0
            if sample_width != 2:
                raise VoiceCloneError("WAV 音频必须是 16-bit")
        else:
            try:
                from mutagen import File as MutagenFile
            except ImportError as exc:
                raise VoiceCloneError("服务端缺少 mutagen，无法检查 MP3/M4A 音频") from exc
            audio = MutagenFile(path)
            if audio is None or not hasattr(audio, "info"):
                raise VoiceCloneError("无法解析音频，请确认文件没有损坏")
            sample_rate = getattr(audio.info, "sample_rate", 0)
            channels = getattr(audio.info, "channels", 0)
            duration = getattr(audio.info, "length", 0)
    except (wave.Error, EOFError, OSError) as exc:
        raise VoiceCloneError("无法解析音频，请确认文件格式和内容正确") from exc

    if not 5 <= duration <= 60:
        raise VoiceCloneError("音频时长需在 5–60 秒之间，推荐 10–20 秒")
    if sample_rate < 16000:
        raise VoiceCloneError("音频采样率不能低于 16 kHz")
    if channels not in (1, 2):
        raise VoiceCloneError("音频必须是单声道或双声道")


def validate_prefix(prefix: str) -> str:
    prefix = prefix.strip()
    if not PREFIX_PATTERN.fullmatch(prefix):
        raise VoiceCloneError("音色前缀只能包含 1–10 位英文字母或数字")
    return prefix


def _response_json(response: requests.Response, operation: str) -> dict[str, Any]:
    try:
        data = response.json()
    except ValueError as exc:
        raise VoiceCloneError(f"{operation}失败：服务端返回了非 JSON 响应") from exc
    if not response.ok:
        message = data.get("message") or data.get("code") or response.text
        request_id = data.get("request_id")
        suffix = f"（request_id: {request_id}）" if request_id else ""
        raise VoiceCloneError(f"{operation}失败：{message}{suffix}")
    return data


class DashScopeVoiceClient:
    def __init__(
        self,
        api_key: str,
        workspace_id: str,
        *,
        timeout: tuple[int, int] = (10, 120),
        session: requests.Session | None = None,
    ) -> None:
        if not api_key:
            raise VoiceCloneError("服务端未配置 DASHSCOPE_API_KEY")
        if not workspace_id:
            raise VoiceCloneError("服务端未配置 DASHSCOPE_WORKSPACE_ID")
        if not re.fullmatch(r"[A-Za-z0-9_-]+", workspace_id):
            raise VoiceCloneError("DASHSCOPE_WORKSPACE_ID 格式不正确")
        self.api_key = api_key
        self.workspace_id = workspace_id
        self.timeout = timeout
        self.session = session or requests.Session()

    @property
    def auth_headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self.api_key}"}

    def upload_temporary_audio(self, path: Path, original_name: str) -> str:
        """Upload to DashScope's 48-hour temporary OSS storage."""
        try:
            response = self.session.get(
                "https://dashscope.aliyuncs.com/api/v1/uploads",
                headers={**self.auth_headers, "Content-Type": "application/json"},
                params={"action": "getPolicy", "model": "voice-enrollment"},
                timeout=self.timeout,
            )
        except requests.RequestException as exc:
            raise VoiceCloneError(f"获取临时上传凭证失败：{exc}") from exc

        policy_body = _response_json(response, "获取临时上传凭证")
        try:
            policy = policy_body["data"]
            safe_name = f"{uuid4().hex}{Path(original_name).suffix.lower()}"
            object_key = f"{policy['upload_dir']}/{safe_name}"
            fields = {
                "OSSAccessKeyId": policy["oss_access_key_id"],
                "Signature": policy["signature"],
                "policy": policy["policy"],
                "x-oss-object-acl": policy["x_oss_object_acl"],
                "x-oss-forbid-overwrite": policy["x_oss_forbid_overwrite"],
                "key": object_key,
                "success_action_status": "200",
            }
            upload_host = policy["upload_host"]
        except KeyError as exc:
            raise VoiceCloneError(f"临时上传凭证缺少字段：{exc.args[0]}") from exc

        try:
            with path.open("rb") as audio:
                upload_response = self.session.post(
                    upload_host,
                    data=fields,
                    files={"file": (safe_name, audio, "application/octet-stream")},
                    timeout=self.timeout,
                )
        except (OSError, requests.RequestException) as exc:
            raise VoiceCloneError(f"上传音频到临时存储失败：{exc}") from exc

        if not upload_response.ok:
            raise VoiceCloneError(
                f"上传音频到临时存储失败：HTTP {upload_response.status_code}"
            )
        return f"oss://{object_key}"

    def create_voice(
        self,
        audio_url: str,
        *,
        prefix: str,
        target_model: str,
        language: str = "zh",
        enable_preprocess: bool = False,
    ) -> CloneResult:
        prefix = validate_prefix(prefix)
        target_model = target_model.strip()
        if not target_model.startswith("qwen-audio-"):
            raise VoiceCloneError("目标模型必须是 qwen-audio 系列模型")

        endpoint = (
            f"https://{self.workspace_id}.cn-beijing.maas.aliyuncs.com"
            "/api/v1/services/audio/tts/customization"
        )
        voice_input: dict[str, Any] = {
            "action": "create_voice",
            "target_model": target_model,
            "prefix": prefix,
            "url": audio_url,
        }
        # The Realtime enrollment API only accepts the four fields above.
        # Language hints and preprocessing are documented for Qwen-Audio-TTS.
        if "-tts-" in target_model:
            voice_input.update(
                language_hints=[language],
                enable_preprocess=enable_preprocess,
            )
        payload = {
            "model": "voice-enrollment",
            "input": voice_input,
        }
        try:
            response = self.session.post(
                endpoint,
                headers={
                    **self.auth_headers,
                    "Content-Type": "application/json",
                    "X-DashScope-OssResourceResolve": "enable",
                },
                json=payload,
                timeout=self.timeout,
            )
        except requests.RequestException as exc:
            raise VoiceCloneError(f"创建音色失败：{exc}") from exc

        body = _response_json(response, "创建音色")
        try:
            voice_id = body["output"]["voice_id"]
        except KeyError as exc:
            raise VoiceCloneError("创建音色响应中没有 output.voice_id") from exc
        if not isinstance(voice_id, str) or not voice_id:
            raise VoiceCloneError("创建音色响应中的 voice_id 无效")
        return CloneResult(voice_id=voice_id, request_id=body.get("request_id"))
