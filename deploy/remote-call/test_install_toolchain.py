"""Exercise the actual installer probe without installing packages or services."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class ToolchainSelectionTests(unittest.TestCase):
    def probe(self, bundled, ambient_toolchain="auto"):
        installer = (Path(__file__).resolve().parents[2] / "scripts/install-relay-centos8.sh").read_text()
        # Execute the actual setup and selection block; exclude all deployment effects.
        preamble = installer.split("root_dir=", 1)[0]
        selection = installer.split("go_bin=''\n", 1)[1].split('if [[ -z $go_bin ]]; then', 1)[0]
        with tempfile.TemporaryDirectory() as directory:
            fake = Path(directory) / "go"
            fake.write_text("""#!/bin/sh
test "$1" = version || exit 91
test -z "${GOROOT:-}" || exit 92
if test "${GOTOOLCHAIN:-auto}" = local; then
    echo 'go version goBUNDLED linux/amd64'
else
    echo 'go version go1.26.5 linux/amd64'
fi
""".replace("BUNDLED", bundled))
            fake.chmod(0o700)
            env = dict(os.environ, PATH=directory + os.pathsep + os.environ["PATH"],
                       GOTOOLCHAIN=ambient_toolchain, GOROOT="/invalid/ambient/goroot")
            result = subprocess.run(["bash", "-c", preamble + "\ngo_bin=''\n" + selection +
                                     "\nprintf '%s' \"$go_bin\""],
                                    env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            return result.stdout == str(fake)

    def test_auto_upgraded_version_does_not_accept_old_bundled_go(self):
        self.assertFalse(self.probe("1.25.3"))

    def test_minimum_bundled_version_is_accepted(self):
        self.assertTrue(self.probe("1.26.3"))

    def test_ambient_toolchain_override_and_goroot_are_cleared(self):
        self.assertTrue(self.probe("1.26.5", "go1.25.3"))


if __name__ == "__main__":
    unittest.main()
