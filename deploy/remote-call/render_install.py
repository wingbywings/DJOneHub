#!/usr/bin/env python3
"""Render a private install bundle; no service/network/system changes. Python 3.6+."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import shlex

MARKER = "# Managed by DJOneHub relay installer."


def legacy_disable_flags(help_file):
    flags = ("no-cli", "no-dtls", "no-tlsv1", "no-tlsv1_1")
    if help_file is None:
        # Preserve the historical standalone renderer's EL8 defaults.
        return flags
    help_text = Path(help_file).read_text()
    supported = set(re.findall(r"^\s*(?:-[A-Za-z0-9?],\s*)?--([A-Za-z0-9_-]+)\b",
                              help_text, re.MULTILINE))
    if "listening-port" not in supported:
        raise ValueError("turnserver help does not contain its listener options")
    return tuple(flag for flag in flags if flag in supported)


def render(args):
    # These paths are inserted into systemd ExecStart, not a shell command.
    # Reject whitespace, specifiers and control characters rather than guess escaping.
    turn_bin = args.turnserver_bin
    disable_flags = legacy_disable_flags(getattr(args, "turnserver_help_file", None))
    if not re.fullmatch(r"/[A-Za-z0-9_./+-]+", turn_bin):
        raise ValueError("turnserver-bin must be an absolute path without systemd special characters")
    domain = args.domain.lower()
    if len(domain) > 253 or not all(
        re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", label)
        for label in domain.split(".")
    ) or "." not in domain:
        raise ValueError("domain must be a DNS hostname, without scheme, port or path")
    public_ip = str(ipaddress.IPv4Address(args.public_ip))
    bind_ip = str(ipaddress.IPv4Address(args.bind_ip))
    if not ipaddress.ip_address(public_ip).is_global:
        raise ValueError("public-ip must be a globally routable IPv4 address")
    if ipaddress.ip_address(bind_ip).is_unspecified or ipaddress.ip_address(bind_ip).is_loopback:
        raise ValueError("bind-ip must be the actual server interface IPv4 address")
    for path in (args.cert, args.key):
        if not Path(path).is_absolute() or any(c in path for c in "\n\r\0"):
            raise ValueError("certificate paths must be absolute")
    urls = ["turn:{}:3478?transport=udp".format(domain),
            "turn:{}:3478?transport=tcp".format(domain),
            "turns:{}:5349?transport=tcp".format(domain)]
    secret, relay_only = secrets.token_hex(32), True
    if args.existing_issuer:
        old = json.loads(Path(args.existing_issuer).read_text())
        if set(old) != {"urls", "secret", "relay_only"} or old["urls"] != urls:
            raise ValueError("existing issuer differs; migrate it explicitly before installing")
        if not isinstance(old["secret"], str) or not re.fullmatch(r"[A-Za-z0-9_-]{32,128}", old["secret"]):
            raise ValueError("invalid existing issuer secret")
        if not isinstance(old["relay_only"], bool):
            raise ValueError("invalid existing relay_only")
        secret, relay_only = old["secret"], old["relay_only"]

    out = Path(args.output)
    out.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(str(out), 0o700)

    def write(name, value):
        with (out / name).open("x") as f:
            f.write(value)
        os.chmod(str(out / name), 0o600)

    write("turn-issuer.json", json.dumps({"urls": urls, "secret": secret,
                                        "relay_only": relay_only}, indent=2) + "\n")
    template = Path(__file__).with_name("turnserver.conf.example").read_text()
    template = template.replace("realm=turn.example.com", "realm=" + domain)
    template += "\n" + "\n".join(disable_flags) + "\n"
    write("turnserver.conf", MARKER + "\n" + template + "\n" +
          "listening-ip={}\nrelay-ip={}\nexternal-ip={}/{}\nstatic-auth-secret={}\n".format(
              bind_ip, bind_ip, public_ip, bind_ip, secret))
    write("deployment.env", MARKER + "\n" + "\n".join(
        key + "=" + shlex.quote(value) for key, value in (
            ("DJ_DOMAIN", domain), ("DJ_CERT_SOURCE", args.cert), ("DJ_KEY_SOURCE", args.key))) + "\n")
    write("djonehub-relay.service", MARKER + """
[Unit]
Description=DJOneHub Relay behind nginx
Wants=network-online.target
After=network-online.target

[Service]
User=djonehub
Group=djonehub
UMask=0077
ExecStart=/usr/local/bin/djonehub-relay serve --state-dir /var/lib/djonehub-relay --listen 127.0.0.1:18443 --insecure-loopback --turn-config /etc/djonehub/turn-issuer.json
Restart=on-failure
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
""")
    write("djonehub-turn.service", MARKER + """
[Unit]
Description=DJOneHub TURN
Wants=network-online.target
After=network-online.target

[Service]
User=djonehub
Group=djonehub
UMask=0077
RuntimeDirectory=djonehub-turn
ExecStart=@TURN_BIN@ -c /etc/djonehub/turnserver.conf --pidfile=/run/djonehub-turn/turnserver.pid --log-file=stdout
Restart=on-failure
RestartSec=3
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
""".replace("@TURN_BIN@", turn_bin))


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for name in ("domain", "public-ip", "bind-ip", "cert", "key", "output"):
        p.add_argument("--" + name, required=True)
    p.add_argument("--existing-issuer")
    p.add_argument("--turnserver-bin", default="/usr/bin/turnserver")
    p.add_argument("--turnserver-help-file",
                   help="Output of the installed turnserver --help; omit unsupported legacy flags")
    try:
        render(p.parse_args())
    except (ValueError, OSError, TypeError) as e:
        # Do not include parsed secret values in errors.
        p.exit(1, "Cannot render deployment: {}\n".format(e))


if __name__ == "__main__":
    main()
