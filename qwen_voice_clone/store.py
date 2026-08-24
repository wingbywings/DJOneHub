from __future__ import annotations

import sqlite3
from pathlib import Path
from typing import Any


SCHEMA = """
CREATE TABLE IF NOT EXISTS voices (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    voice_id TEXT NOT NULL UNIQUE,
    prefix TEXT NOT NULL,
    target_model TEXT NOT NULL,
    original_filename TEXT NOT NULL,
    request_id TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
)
"""


class VoiceStore:
    def __init__(self, path: str | Path) -> None:
        self.path = str(path)
        Path(self.path).parent.mkdir(parents=True, exist_ok=True)
        with self._connect() as connection:
            connection.execute(SCHEMA)

    def _connect(self) -> sqlite3.Connection:
        connection = sqlite3.connect(self.path)
        connection.row_factory = sqlite3.Row
        return connection

    def add(
        self,
        *,
        voice_id: str,
        prefix: str,
        target_model: str,
        original_filename: str,
        request_id: str | None,
    ) -> dict[str, Any]:
        with self._connect() as connection:
            cursor = connection.execute(
                """
                INSERT INTO voices
                    (voice_id, prefix, target_model, original_filename, request_id)
                VALUES (?, ?, ?, ?, ?)
                """,
                (voice_id, prefix, target_model, original_filename, request_id),
            )
            row = connection.execute(
                "SELECT * FROM voices WHERE id = ?", (cursor.lastrowid,)
            ).fetchone()
        return dict(row)

    def list(self) -> list[dict[str, Any]]:
        with self._connect() as connection:
            rows = connection.execute(
                "SELECT * FROM voices ORDER BY created_at DESC, id DESC"
            ).fetchall()
        return [dict(row) for row in rows]

