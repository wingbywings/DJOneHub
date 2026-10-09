"""Exercise route setup with isolated CLI/HTTP stand-ins; never touch Keychain/modems."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class ConfigureRemoteTests(unittest.TestCase):
    def run_setup(self, *, scope="dji-real", module="dji-real", phones=1, active=None, check=False, http_error=False):
        script = Path(__file__).resolve().parents[2] / "scripts/configure-remote-answering.sh"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fixture = {"scope": scope, "phones": phones, "active": active, "http_error": http_error}
            (root / "fixture.json").write_text(json.dumps(fixture))
            native = root / "native"
            native.write_text("#!" + sys.executable + "\n" + """
import json, os, sys
from pathlib import Path
f = json.loads((Path(os.environ['DJ_FIXTURE']) / 'fixture.json').read_text())
if sys.argv[2] == 'status':
    print(json.dumps({'phase':'ready','principal':{'modules':[f['scope']]}}))
elif sys.argv[2] == 'installations':
    print(json.dumps([{'id':'ios_'+str(i),'modules':[f['scope']]} for i in range(f['phones'])]))
else:
    raise SystemExit(1)
""")
            native.chmod(0o700)
            curl = root / "curl"
            curl.write_text("#!" + sys.executable + "\n" + """
import json, os, sys
from pathlib import Path
root = Path(os.environ['DJ_FIXTURE'])
f = json.loads((root / 'fixture.json').read_text())
args = sys.argv[1:]
method = args[args.index('-X')+1]
url = args[args.index('-X')+2]
body = None
if '--data-binary' in args:
    body = json.loads(Path(args[args.index('--data-binary')+1][1:]).read_text())
with (root/'requests').open('a') as out:
    out.write(json.dumps({'method':method,'url':url,'body':body})+'\\n')
response, code = {}, 200
if url.endswith('/api/devices'):
    response = [{'id':'dji-real','alias':'Xiaomi','state':'ready'}]
elif url.endswith('/calls/status'):
    response = {'active':f['active']}
elif url.endswith('/remote-calls/status'):
    response = {'online_transport':{'configured':True,'state':'online'},
                'capabilities':{'remote_media':True},'codec':{'available':True}}
    if (root/'configured').exists():
        response.update(effective_route='remote',config=json.loads((root/'configured').read_text()))
elif url.endswith('/voice-agent/status'):
    response = {'state':{'enabled':True,'provider':'qwen','instructions':'keep this','revision':23}}
elif method == 'PUT' and url.endswith('/remote-calls/config'):
    (root/'configured').write_text(json.dumps(body))
if f['http_error'] and url.endswith('/calls/status'):
    response, code = {'error':'device is offline or unavailable'}, 409
Path(args[args.index('-o')+1]).write_text(json.dumps(response))
print(code, end='')
""")
            curl.chmod(0o700)
            env = dict(os.environ, DJ_FIXTURE=directory,
                       PATH=directory + os.pathsep + os.environ["PATH"])
            args = ["bash", str(script), "--binary", str(native), "--module", module]
            if check:
                args.append("--check-only")
            result = subprocess.run(args, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True)
            log = root / "requests"
            requests = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
            return result, requests

    def test_wrong_scope_stops_before_mutation(self):
        result, requests = self.run_setup(scope="dji-xiaomi")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not authorize dji-real", result.stderr)
        self.assertFalse(any(x["method"] == "PUT" for x in requests))

    def test_placeholder_stops_before_any_http(self):
        result, requests = self.run_setup(module="dji-REPLACE")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(requests, [])

    def test_multiple_phones_require_selection(self):
        result, requests = self.run_setup(phones=2)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(x["method"] == "PUT" for x in requests))

    def test_active_call_stops_configuration(self):
        result, requests = self.run_setup(active={"state":"incoming"})
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("End the current call", result.stderr)
        self.assertFalse(any(x["method"] == "PUT" for x in requests))

    def test_api_error_preserves_diagnostic_and_stops(self):
        result, requests = self.run_setup(http_error=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("HTTP 409", result.stderr)
        self.assertIn("device is offline or unavailable", result.stderr)
        self.assertFalse(any(x["method"] == "PUT" for x in requests))

    def test_check_only_never_changes_routes(self):
        result, requests = self.run_setup(check=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(x["method"] == "PUT" for x in requests))

    def test_valid_setup_preserves_ai_fields_and_uses_real_installation(self):
        result, requests = self.run_setup()
        self.assertEqual(result.returncode, 0, result.stderr)
        writes = [x for x in requests if x["method"] == "PUT"]
        self.assertEqual(len(writes), 2)
        self.assertEqual(writes[0]["body"], {"enabled":False,"provider":"qwen","instructions":"keep this"})
        self.assertEqual(writes[1]["body"]["target_installation_id"], "ios_0")
        self.assertTrue(writes[1]["body"]["enabled"])


if __name__ == "__main__":
    unittest.main()
