// Exercise authenticated phone selection with delayed reads and module changes.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source = fs.readFileSync(new URL('../cmd/djonehub-macos/web/app.js', import.meta.url), 'utf8');
const block = source.slice(source.indexOf('let remoteLoadID ='), source.indexOf('async function loadVoiceAgentStatus('));
const nodes = new Map();
function makeNode() { return { value: '', disabled: false, children: [], append(child) { this.children.push(child); }, replaceChildren(...items) { this.children = items; }, addEventListener(event, fn) { this[event] = fn; } }; }
function $(id) { if (!nodes.has(id)) nodes.set(id, makeNode()); return nodes.get(id); }
const reads = [], writes = [], notices = [];
let authenticated = true, rejectSave;
const tick = () => new Promise(setImmediate);
const phones = ['A','B','C'].map(id => ({ id: 'phone_' + id, hub_id: 'hub_active', modules: [id], name: '同名手机', revoked: false }));
const reply = data => ({ ok: true, async json() { return data; } });
const context = vm.createContext({ $, activeDeviceID: 'A', Date, setTimeout, document: { createElement: makeNode }, notice(text) { notices.push(text); },
  api(path) {
    if (path === '/api/remote-management/session/status') return Promise.resolve({ authenticated, csrf: 'fixture-csrf' });
    if (path === '/api/remote-management/profiles') return Promise.resolve({ active_profile: 'fixture', profiles: [{ profile: 'fixture', status: { principal: { id: 'hub_active' } } }] });
    return new Promise((resolve, reject) => reads.push({ path, resolve, reject }));
  },
  fetch(path, options) {
    if (options?.method === 'POST') { const body = JSON.parse(options.body); writes.push({ path, body, options }); if (path.endsWith('/routes')) return new Promise((resolve, reject) => { rejectSave = reject; }); return Promise.resolve(reply({ id: 'list-job' })); }
    return Promise.resolve(reply({ state: 'completed', result: { installations: phones } }));
  }
});
vm.runInContext(block, context);
const state = id => ({ config: { enabled: true, target_installation_id: id, ring_timeout_seconds: 45, caller_display: 'module' }, effective_route: 'remote' });
const a = context.loadRemoteSettings(); context.activeDeviceID = 'B'; const b = context.loadRemoteSettings();
reads[1].resolve(state('phone_B')); await b;
reads[0].resolve(state('phone_A')); await a;
assert.equal($('#remote-installation').value, 'phone_B');
assert.equal($('#remote-installation').children.length, 2); // only phone B + placeholder
const save = $('#remote-form').submit({ preventDefault() {} }); await tick();
assert.equal(writes[1].path, '/api/remote-management/routes');
assert.equal(writes[1].body.module, 'B'); assert.equal(writes[1].body.config.target_installation_id, 'phone_B');
assert.equal(writes[1].options.headers['X-DJOneHub-CSRF'], 'fixture-csrf');
context.activeDeviceID = 'C'; const c = context.loadRemoteSettings(); reads[2].resolve(state('phone_C')); await c;
rejectSave(new Error('late B failure')); await save;
assert.equal($('#remote-installation').value, 'phone_C'); assert.equal($('#remote-fields').disabled, false);
const before = writes.length; $('#remote-installation').value = 'phone_B'; await $('#remote-form').submit({ preventDefault() {} }); assert.equal(writes.length, before); assert.match(notices.at(-1), /有效授权/);
authenticated = false; const locked = context.loadRemoteSettings(); reads[3].resolve(state('phone_C')); await locked;
assert.equal($('#remote-fields').disabled, true); assert.match($('#remote-status').textContent, /登录本机管理/);
console.log('Remote settings: authenticated module selection, stale reads, bound writes and login guard passed');
