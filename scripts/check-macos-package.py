#!/usr/bin/env python3
"""Smoke-test a staged Mac package without USB, Keychain or fixed ports."""
import json
import pathlib
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def check(stage):
    backend = stage / "bin/djonehub-macos"
    binaries = [backend, stage / "bin/djonehub-audio-host", stage / "bin/djonehub-remote", stage / "lib/libusb-1.0.0.dylib"]
    for binary in binaries:
        links = subprocess.check_output(["otool", "-L", str(binary)], text=True)
        assert not any(path in links for path in ("/opt/homebrew", "/usr/local", "/Cellar/")), links
        subprocess.run(["codesign", "--verify", "--strict", str(binary)], check=True)
    for notice in ("libusb-COPYING", "opus-COPYING"):
        assert (stage / "licenses" / notice).stat().st_size > 0
    help_text = subprocess.check_output([str(stage / "djonehub"), "remote", "help"], text=True)
    assert "media-check" in help_text
    for args in (["start", "--remote-profile"], ["start", "--demo", "unexpected"]):
        result = subprocess.run([str(stage / "djonehub"), *args], stdout=subprocess.DEVNULL)
        assert result.returncode == 2

    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with tempfile.TemporaryFile() as log:
        process = subprocess.Popen([str(backend), "--demo", "--listen", f"127.0.0.1:{port}"], stdout=log, stderr=log)
        try:
            deadline = time.monotonic() + 15
            while True:
                if process.poll() is not None:
                    raise RuntimeError("demo backend exited before readiness")
                try:
                    with opener.open(f"http://127.0.0.1:{port}/api/remote-calls/status", timeout=1) as response:
                        status = json.load(response)
                    break
                except urllib.error.URLError:
                    if time.monotonic() >= deadline:
                        raise RuntimeError("demo backend readiness timed out")
                    time.sleep(.1)
            assert status["codec"]["available"] is True, status["codec"]
            assert "1.6.1" in status["codec"]["version"], status["codec"]
            print("PASS: packaged remoteopus available; Opus 1.6.1; bundled dependencies/signatures/licenses; launcher arguments")
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: check-macos-package.py /absolute/staged-package")
    check(pathlib.Path(sys.argv[1]).resolve())
