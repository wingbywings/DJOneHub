import json
import os
from pathlib import Path
import sqlite3
import tempfile
import unittest

from upgrade_apns import BASE, APNS, backup_database, health, validate_command


class UpgradeAPNsTests(unittest.TestCase):
    def test_existing_service_must_match_without_losing_flags(self):
        for command in (BASE, BASE + APNS):
            status = "{ path=/usr/local/bin/djonehub-relay ; argv[]=" + command + " ; ignore_errors=no ; pid=12 ; status=0/0 }"
            unit = validate_command(status)
            self.assertIn("ExecStart=\nExecStart=" + BASE + APNS, unit)
        for command in (BASE + " --extra secret", BASE.replace("18443", "8443"), BASE.replace("serve", "serve\nExecStart=/bin/evil")):
            with self.assertRaises(ValueError):
                validate_command("{ path=/usr/local/bin/djonehub-relay ; argv[]=" + command + " ; pid=12 }")

    def test_private_offline_snapshot_includes_committed_wal_and_rejects_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "identities.sqlite"
            target = Path(directory) / "backup.sqlite"
            connection = sqlite3.connect(str(source))
            self.addCleanup(connection.close)
            connection.execute("PRAGMA journal_mode=WAL")
            connection.executescript("PRAGMA user_version=1; CREATE TABLE identities(id TEXT); INSERT INTO identities VALUES('retained');")
            connection.commit()
            self.assertTrue(Path(str(source) + "-wal").exists())
            backup_database(source, target)
            self.assertEqual(target.stat().st_mode & 0o777, 0o600)
            result = sqlite3.connect(str(target))
            self.addCleanup(result.close)
            self.assertEqual(result.execute("SELECT id FROM identities").fetchall(), [("retained",)])
            self.assertEqual(result.execute("PRAGMA user_version").fetchone(), (1,))
            before = target.read_bytes()
            with self.assertRaises(ValueError):
                backup_database(source, target)
            self.assertEqual(before, target.read_bytes())
            link = Path(directory) / "link.sqlite"
            link.symlink_to(source)
            with self.assertRaises(ValueError):
                backup_database(link, Path(directory) / "bad.sqlite")

    def test_health_requires_push_and_preserves_existing_features(self):
        caps = dict(identity=True, signaling=True, outgoing_calls=True, voip_push=True, turn=True)
        health(json.dumps(dict(capabilities=caps)))
        for key in caps:
            wrong = dict(caps)
            wrong[key] = False
            with self.assertRaises(ValueError):
                health(json.dumps(dict(capabilities=wrong)))


if __name__ == "__main__":
    unittest.main()
