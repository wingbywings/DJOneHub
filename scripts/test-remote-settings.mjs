// Exercise the shipped settings controller with delayed requests and module switches.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source = fs.readFileSync(new URL('../cmd/djonehub-macos/web/app.js', import.meta.url), 'utf8');
const block = source.slice(source.indexOf('let remoteLoadID ='), source.indexOf('async function loadVoiceAgentStatus('));
const nodes = new Map();
function $(id) {
  if (!nodes.has(id)) nodes.set(id, { value: '', disabled: false, addEventListener(event, fn) { this[event] = fn; } });
  return nodes.get(id);
}
const requests = [];
const context = vm.createContext({ $, activeDeviceID: 'A', notice() {},
  routedAPIPath(path) { return `/api/devices/${context.activeDeviceID}${path.slice(4)}`; },
  api(path, options) { return new Promise((resolve, reject) => requests.push({ path, options, resolve, reject })); }
});
vm.runInContext(block, context);
const state = id => ({ config: { enabled: true, target_installation_id: id, ring_timeout_seconds: 45, caller_display: 'module' }, effective_route: 'remote' });
const a = context.loadRemoteSettings();
context.activeDeviceID = 'B';
const b = context.loadRemoteSettings();
requests[1].resolve(state('phone_B')); await b;
requests[0].resolve(state('phone_A')); await a;
assert.equal($('#remote-installation').value, 'phone_B');
const save = $('#remote-form').submit({ preventDefault() {} });
assert.equal(requests[2].path, '/api/devices/B/remote-calls/config');
assert.equal(JSON.parse(requests[2].options.body).target_installation_id, 'phone_B');
context.activeDeviceID = 'C';
const c = context.loadRemoteSettings();
requests[3].resolve(state('phone_C')); await c;
requests[2].reject(new Error('late B failure')); await save;
assert.equal($('#remote-installation').value, 'phone_C');
assert.equal($('#remote-fields').disabled, false);
console.log('Remote settings: stale read and module-bound write passed');
