#!/usr/bin/env python3
"""Strict helpers for the existing installer layout. No secrets are printed."""
import json
import os
from pathlib import Path
import re
import shlex
from urllib.parse import urlsplit
import sqlite3
import shutil
import sys

BASE = "/usr/local/bin/djonehub-relay serve --state-dir /var/lib/djonehub-relay --listen 127.0.0.1:18443 --insecure-loopback --turn-config /etc/djonehub/turn-issuer.json"
APNS = " --apns-config /etc/djonehub/apns.json"


def validate_command(value):
    # systemctl exposes path/argv plus runtime timestamps. Do not interpret
    # arbitrary shell commands, unit specifiers or unknown command options.
    match = re.fullmatch(r"\{ path=/usr/local/bin/djonehub-relay ; argv\[\]=(.*?) ; .* \}", value.strip())
    if not match:
        raise ValueError("Existing service differs from the supported installer layout")
    command = shlex.split(match.group(1))
    base = shlex.split(BASE)
    if command[:len(base)] != base:
        raise ValueError("Existing service differs from the supported installer layout")
    trailing = command[len(base):]
    origin = None
    admin = {}
    seen = set()
    while trailing:
        if len(trailing) < 2:
            raise ValueError("Missing managed option value")
        option, value, trailing = trailing[0], trailing[1], trailing[2:]
        if option in seen:
            raise ValueError("Duplicate managed option")
        seen.add(option)
        if option == "--apns-config" and value == "/etc/djonehub/apns.json":
            continue
        if option == "--public-origin":
            parsed = urlsplit(value)
            if (parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password
                    or parsed.query or parsed.fragment or parsed.path not in ("", "/")
                    or not re.fullmatch(r"https://[A-Za-z0-9.:\[\]-]+/?", value)):
                raise ValueError("Invalid managed enrollment origin")
            origin = value
            continue
        if option == "--admin-state-dir":
            if not re.fullmatch(r"/[A-Za-z0-9_./-]+", value) or Path(value).as_posix() != os.path.normpath(value):
                raise ValueError("Invalid managed administration directory")
            admin[option] = value
            continue
        if option == "--admin-tenant":
            if not re.fullmatch(r"[A-Za-z0-9_-]{1,128}", value):
                raise ValueError("Invalid managed administration tenant")
            admin[option] = value
            continue
        if option == "--admin-path":
            if not re.fullmatch(r"/[a-z][a-z0-9-]{0,31}", value) or value in ("/v1", "/healthz"):
                raise ValueError("Invalid managed administration path")
            admin[option] = value
            continue
        raise ValueError("Unknown managed option; preserve it and migrate manually")
    if admin and (not origin or "--admin-state-dir" not in admin or "--admin-tenant" not in admin):
        raise ValueError("Incomplete integrated administration configuration")
    suffix = " --public-origin " + origin if origin else ""
    suffix += "".join(" " + option + " " + admin[option] for option in ("--admin-state-dir", "--admin-tenant", "--admin-path") if option in admin)
    return "# Managed by DJOneHub APNs upgrade.\n[Service]\nExecStart=\nExecStart=" + BASE + APNS + suffix + "\n"



def backup_database(source, destination):
    source, destination = Path(source), Path(destination)
    if not source.is_absolute() or not destination.is_absolute() or source.is_symlink() or not source.is_file():
        raise ValueError("Invalid database path")
    # Relay is stopped by the shell. Checkpoint committed WAL first, then
    # copy the quiescent database. This also supports CentOS 8 Python 3.6,
    # whose sqlite3 module does not yet expose Connection.backup().
    if destination.exists() or destination.is_symlink():
        raise ValueError("Backup destination already exists")
    read = sqlite3.connect(source.as_uri() + "?mode=rw", uri=True)
    try:
        if read.execute("PRAGMA quick_check").fetchone() != ("ok",):
            raise ValueError("Source database integrity check failed")
        version = read.execute("PRAGMA user_version").fetchone()[0]
        if version not in (1, 2, 3):
            raise ValueError("Unsupported database version")
        checkpoint = read.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone()
        if checkpoint is None or checkpoint[0] != 0:
            raise ValueError("Database is still busy; cannot snapshot")
    finally:
        read.close()
    fd = os.open(str(destination), os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    with os.fdopen(fd, "wb") as output, source.open("rb") as original:
        shutil.copyfileobj(original, output)
        output.flush()
        os.fsync(output.fileno())
    verify = sqlite3.connect(destination.as_uri() + "?mode=ro", uri=True)
    try:
        if verify.execute("PRAGMA quick_check").fetchone() != ("ok",):
            raise ValueError("Backup database integrity check failed")
    finally:
        verify.close()
    print("Verified private SQLite backup; schema={}".format(version))


def health(value):
    caps = json.loads(value).get("capabilities", {})
    if not all(caps.get(key) is True for key in ("identity", "signaling", "outgoing_calls", "voip_push", "turn")):
        raise ValueError("Relay does not advertise required capabilities")


def main():
    try:
        mode = sys.argv[1]
        if mode == "unit":
            print(validate_command(sys.stdin.read()), end="")
        elif mode == "backup":
            backup_database(sys.argv[2], sys.argv[3])
        elif mode == "health":
            health(sys.stdin.read())
        else:
            raise ValueError("Unknown operation")
    except (ValueError, OSError, sqlite3.Error, IndexError):
        sys.exit("APNs upgrade preflight/backup/health check failed; no credential values logged")


if __name__ == "__main__":
    main()
