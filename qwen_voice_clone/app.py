from __future__ import annotations

import os
import tempfile
from pathlib import Path

from flask import Flask, jsonify, render_template, request
from werkzeug.exceptions import RequestEntityTooLarge
from werkzeug.utils import secure_filename

from .service import (
    DEFAULT_TARGET_MODEL,
    MAX_AUDIO_BYTES,
    DashScopeVoiceClient,
    VoiceCloneError,
    validate_audio,
    validate_prefix,
)
from .store import VoiceStore


def create_app(config: dict | None = None) -> Flask:
    app = Flask(__name__)
    app.config.from_mapping(
        MAX_CONTENT_LENGTH=MAX_AUDIO_BYTES + 512 * 1024,
        VOICE_DB_PATH=os.getenv(
            "VOICE_DB_PATH", str(Path(__file__).with_name("data") / "voices.sqlite3")
        ),
        DASHSCOPE_API_KEY=os.getenv("DASHSCOPE_API_KEY", ""),
        DASHSCOPE_WORKSPACE_ID=os.getenv("DASHSCOPE_WORKSPACE_ID", ""),
    )
    if config:
        app.config.update(config)

    store = VoiceStore(app.config["VOICE_DB_PATH"])
    client_factory = app.config.get("VOICE_CLIENT_FACTORY") or (
        lambda: DashScopeVoiceClient(
            app.config["DASHSCOPE_API_KEY"], app.config["DASHSCOPE_WORKSPACE_ID"]
        )
    )

    @app.get("/")
    def index():
        return render_template(
            "index.html",
            default_target_model=DEFAULT_TARGET_MODEL,
            configured=bool(
                app.config["DASHSCOPE_API_KEY"]
                and app.config["DASHSCOPE_WORKSPACE_ID"]
            ),
        )

    @app.get("/api/voices")
    def list_voices():
        return jsonify({"voices": store.list()})

    @app.post("/api/voices")
    def create_voice():
        audio = request.files.get("audio")
        if audio is None or not audio.filename:
            raise VoiceCloneError("请选择要上传的音频文件")

        prefix = validate_prefix(request.form.get("prefix", ""))
        target_model = request.form.get("target_model", DEFAULT_TARGET_MODEL).strip()
        language = request.form.get("language", "zh").strip() or "zh"
        enable_preprocess = request.form.get("enable_preprocess") == "true"
        original_filename = secure_filename(audio.filename) or "voice.wav"

        suffix = Path(original_filename).suffix.lower()
        temporary_path: Path | None = None
        try:
            with tempfile.NamedTemporaryFile(suffix=suffix, delete=False) as temporary:
                temporary_path = Path(temporary.name)
                audio.save(temporary)
            validate_audio(temporary_path, original_filename, temporary_path.stat().st_size)

            client = client_factory()
            audio_url = client.upload_temporary_audio(temporary_path, original_filename)
            result = client.create_voice(
                audio_url,
                prefix=prefix,
                target_model=target_model,
                language=language,
                enable_preprocess=enable_preprocess,
            )
            saved = store.add(
                voice_id=result.voice_id,
                prefix=prefix,
                target_model=target_model,
                original_filename=original_filename,
                request_id=result.request_id,
            )
            return jsonify({"voice": saved}), 201
        finally:
            if temporary_path is not None:
                temporary_path.unlink(missing_ok=True)

    @app.errorhandler(VoiceCloneError)
    def handle_clone_error(error: VoiceCloneError):
        return jsonify({"error": str(error)}), 400

    @app.errorhandler(RequestEntityTooLarge)
    def handle_large_file(_error: RequestEntityTooLarge):
        return jsonify({"error": "音频文件不能超过 10 MB"}), 413

    return app


if __name__ == "__main__":
    app = create_app()
    app.run(
        host=os.getenv("HOST", "0.0.0.0"),
        port=int(os.getenv("PORT", "7576")),
        debug=False,
    )
