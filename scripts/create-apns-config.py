#!/usr/bin/env python3
"""Create a new private Relay APNs JSON file without printing key material."""
import argparse
import json
import os
from pathlib import Path
import re
import stat
import subprocess


def create(args):
    if not re.fullmatch(r"[A-Z0-9]{10}", args.team_id) or not re.fullmatch(r"[A-Z0-9]{10}", args.key_id):
        raise ValueError("Team ID and Key ID must each contain 10 uppercase letters/digits")
    if not re.fullmatch(r"[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+", args.bundle_id) or len(args.bundle_id) > 200:
        raise ValueError("invalid Bundle ID; supply the app ID without the .voip suffix")
    source, destination = Path(args.p8), Path(args.out)
    if not source.is_absolute() or not destination.is_absolute():
        raise ValueError("--p8 and --out must be absolute paths")
    fd = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as key_file:
        info = os.fstat(key_file.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > 4096:
            raise ValueError("the .p8 must be a regular file no larger than 4 KiB")
        pem = key_file.read(4097)
    if not pem.startswith(b"-----BEGIN PRIVATE KEY-----"):
        raise ValueError("expected an unencrypted PKCS#8 APNs .p8 key")
    # Inspect only the public key. Never include OpenSSL diagnostics or private
    # key bytes in an exception, stdout, process arguments, or output log.
    public = subprocess.run(["openssl", "pkey", "-pubout", "-outform", "DER"], input=pem,
                            capture_output=True, timeout=5, check=False)
    # SubjectPublicKeyInfo algorithm sequence: id-ecPublicKey + prime256v1.
    p256_prefix = bytes.fromhex("3059301306072a8648ce3d020106082a8648ce3d03010703420004")
    if public.returncode or len(public.stdout) != 91 or not public.stdout.startswith(p256_prefix):
        raise ValueError("expected an EC P-256 APNs key")
    config = dict(team_id=args.team_id, key_id=args.key_id, bundle_id=args.bundle_id,
                  environment=args.environment, private_key_pem=pem.decode("ascii"))
    data = (json.dumps(config, indent=2) + "\n").encode("utf-8")
    if len(data) > 8192:
        raise ValueError("configuration exceeds Relay limit")
    fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, "wb") as output:
            os.fchmod(output.fileno(), 0o600)
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
    except BaseException:
        destination.unlink(missing_ok=True)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--p8", required=True, help="absolute path to downloaded APNs .p8 key")
    parser.add_argument("--team-id", required=True)
    parser.add_argument("--key-id", required=True)
    parser.add_argument("--bundle-id", required=True, help="existing app Bundle ID, without .voip")
    parser.add_argument("--environment", required=True, choices=["sandbox", "production"])
    parser.add_argument("--out", required=True, help="new absolute output path; never overwrites")
    args = parser.parse_args()
    try:
        create(args)
    except ValueError as error:
        parser.exit(1, f"{error}\n")
    except (OSError, subprocess.SubprocessError):
        parser.exit(1, "Could not read key, validate using OpenSSL, or create a new private output file.\n")
    print("Created private APNs JSON (0600). Keep it outside Git; only the Relay needs this file.")


if __name__ == "__main__":
    main()
