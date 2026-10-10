#!/usr/bin/env python3
"""Package only allowlisted Relay sources; never include dist runtime secrets."""
import hashlib
from pathlib import Path
import tarfile

root = Path(__file__).resolve().parent.parent
output = root / "dist/djonehub-relay-apns-source.tar.gz"
entries = ["go.mod", "go.sum", "cmd/djonehub-relay", "internal", "pkg", "third_party",
           "deploy/remote-call", "scripts/upgrade-relay-apns.sh",
           "docs/REMOTE_APNS_UPGRADE.md", "docs/remote-calls-thirteenth-batch.md"]
files = []
for entry in entries:
    path = root / entry
    for item in ([path] if path.is_file() else path.rglob("*")):
        relative = item.relative_to(root)
        if any(part in (".git", "__pycache__", ".build", "node_modules") for part in relative.parts):
            continue
        if item.is_symlink():
            raise SystemExit("Refusing symlink in source bundle")
        if not item.is_file() or item.name == ".DS_Store":
            continue
        if item.suffix in (".p8", ".sqlite", ".db", ".pyc", ".log"):
            raise SystemExit("Unexpected private/runtime file in source tree")
        files.append(item)
output.parent.mkdir(exist_ok=True)
with tarfile.open(output, "w:gz") as archive:
    for item in sorted(files):
        info = archive.gettarinfo(str(item), arcname="djonehub-relay-apns/" + item.relative_to(root).as_posix())
        info.uid = info.gid = 0
        info.uname = info.gname = ""
        with item.open("rb") as source:
            archive.addfile(info, source)
checksum = hashlib.sha256(output.read_bytes()).hexdigest()
output.with_name(output.name + ".sha256").write_text(checksum + "  " + output.name + "\n")
print("Packaged {} source files; SHA-256 {}".format(len(files), checksum))
