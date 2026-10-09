import argparse
import json
import os
from pathlib import Path
import shlex
import tempfile
import unittest

from render_install import render


class InstallRenderTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.base = Path(self.tmp.name)

    def args(self, name="new", **changes):
        values = dict(domain="relay.example.com", public_ip="8.8.8.8", bind_ip="10.0.0.5",
                      cert="/existing site's/fullchain.pem", key="/existing site's/key.pem",
                      output=str(self.base / name), existing_issuer=None, turnserver_bin="/usr/bin/turnserver")
        values.update(changes)
        return argparse.Namespace(**values)

    def test_private_bundle_matching_secret_and_nat(self):
        args = self.args()
        render(args)
        out = Path(args.output)
        config = json.loads((out / "turn-issuer.json").read_text())
        turn = (out / "turnserver.conf").read_text()
        self.assertIn("static-auth-secret=" + config["secret"], turn)
        self.assertIn("external-ip=8.8.8.8/10.0.0.5", turn)
        self.assertIn("listening-ip=10.0.0.5", turn)
        self.assertTrue(config["relay_only"])
        self.assertEqual(len(config["urls"]), 3)
        for path in out.iterdir():
            self.assertEqual(os.stat(str(path)).st_mode & 0o777, 0o600)
        self.assertEqual(os.stat(str(out)).st_mode & 0o777, 0o700)
        unit = (out / "djonehub-relay.service").read_text()
        self.assertIn("--listen 127.0.0.1:18443 --insecure-loopback", unit)
        self.assertNotIn("--tls-key", unit)
        env = (out / "deployment.env").read_text().splitlines()
        self.assertEqual(shlex.split(env[2].split("=", 1)[1]), [args.cert])

    def test_rerender_preserves_secret_and_relay_policy(self):
        first = self.args()
        render(first)
        old_path = Path(first.output) / "turn-issuer.json"
        old = json.loads(old_path.read_text())
        old["relay_only"] = False
        old_path.write_text(json.dumps(old))
        second = self.args("second", existing_issuer=str(old_path))
        render(second)
        self.assertEqual(json.loads((Path(second.output) / "turn-issuer.json").read_text()), old)

    def test_installed_turn_options_preserve_legacy_or_modern_defaults(self):
        legacy = ("no-cli", "no-dtls", "no-tlsv1", "no-tlsv1_1")
        for name, flags in (("legacy", legacy), ("modern", ()), ("mixed", ("no-dtls",))):
            with self.subTest(name=name):
                help_file = self.base / (name + "-help.txt")
                help_file.write_text(" --listening-port\tPort\n --cli\tEnable CLI\n" +
                                     "".join(" --" + flag + "\tDisable\n" for flag in flags))
                args = self.args(name, turnserver_help_file=str(help_file))
                render(args)
                lines = (Path(args.output) / "turnserver.conf").read_text().splitlines()
                for flag in legacy:
                    self.assertEqual(flag in lines, flag in flags)
                for flag in ("cli", "dtls", "tlsv1", "tlsv1_1", "no-auth"):
                    self.assertNotIn(flag, lines)

    def test_invalid_turn_help_does_not_create_private_bundle(self):
        help_file = self.base / "bad-help.txt"
        help_file.write_text("permission denied")
        args = self.args(turnserver_help_file=str(help_file))
        with self.assertRaises(ValueError):
            render(args)
        self.assertFalse(Path(args.output).exists())

    def test_different_host_does_not_silently_replace_identity(self):
        render(self.args())
        args = self.args("second", domain="other.example.com",
                         existing_issuer=str(self.base / "new/turn-issuer.json"))
        with self.assertRaises(ValueError):
            render(args)
        self.assertFalse(Path(args.output).exists())

    def test_turn_service_uses_the_detected_binary_path(self):
        for index, binary in enumerate(("/usr/sbin/turnserver", "/usr/local/bin/turnserver", "/bin/turnserver")):
            with self.subTest(binary=binary):
                args = self.args(str(index), turnserver_bin=binary)
                render(args)
                unit = (Path(args.output) / "djonehub-turn.service").read_text()
                self.assertIn("ExecStart=" + binary + " -c ", unit)
                self.assertNotIn("@TURN_BIN@", unit)

    def test_turn_service_rejects_command_or_specifier_injection(self):
        for binary in ("turnserver", "/usr/bin/turnserver --no-auth", "/tmp/%u/turnserver", "/tmp/turn\nExecStart=bad"):
            with self.subTest(binary=binary):
                with self.assertRaises(ValueError):
                    render(self.args(turnserver_bin=binary))
                self.assertFalse((self.base / "new").exists())

    def test_invalid_domain_address_or_path_creates_no_files(self):
        for changes in [dict(domain="example.com\nno-auth"), dict(domain="https://example.com"),
                        dict(public_ip="10.0.0.1"), dict(bind_ip="127.0.0.1"),
                        dict(cert="relative.pem"), dict(key="/tmp/key\nmalicious")]:
            with self.subTest(changes=changes):
                with self.assertRaises(ValueError):
                    render(self.args(**changes))
                self.assertFalse((self.base / "new").exists())

    def test_bad_existing_credentials_do_not_leak_in_error(self):
        render(self.args())
        path = self.base / "new/turn-issuer.json"
        old = json.loads(path.read_text())
        old["secret"] = "SECRET\nno-auth"
        path.write_text(json.dumps(old))
        with self.assertRaises(ValueError) as result:
            render(self.args("second", existing_issuer=str(path)))
        self.assertNotIn("SECRET", str(result.exception))

    def test_nginx_preserves_protocol_path_and_disables_replay(self):
        snippet = Path(__file__).with_name("nginx-relay.locations.conf").read_text()
        self.assertIn("location = /v1/signaling", snippet)
        self.assertIn("proxy_set_header Upgrade $http_upgrade", snippet)
        self.assertIn('proxy_set_header Connection "upgrade"', snippet)
        self.assertEqual(snippet.count("proxy_pass http://127.0.0.1:18443;"), 3)
        self.assertEqual(snippet.count("proxy_next_upstream off;"), 3)
        self.assertNotIn("ssl_certificate", snippet)


if __name__ == "__main__":
    unittest.main()
