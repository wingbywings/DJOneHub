// Exercise module-switch races and setup recovery notices in the actual UI loader.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source = fs.readFileSync(new URL('../cmd/djonehub-macos/web/app.js', import.meta.url), 'utf8');
const loader = source.slice(source.indexOf('async function loadVoiceSetup()'), source.indexOf('function profileRows('));
const nodes = new Map();
const $ = id => { if (!nodes.has(id)) nodes.set(id, {}); return nodes.get(id); };
const reads = [];
const context = vm.createContext({ $, activeDeviceID: '5ber1', voiceSetupInFlight: false, currentVoiceSetup: {},
  api(path) { return new Promise((resolve, reject) => reads.push({ path, resolve, reject })); }
});
vm.runInContext(loader, context);
const voice = { runtime_installed: true, runtime_detail: '已校验' };
const pending = { state: 'rollback_verifying', stage: 'rollback_verify', summary: '等待重连', original_error: 'IMS 回读不一致', restart_error: 'LIBUSB_ERROR_IO', detail: '原端口尚未重新枚举', can_initialize: true };
let load = context.loadVoiceSetup();
reads[0].resolve(pending); reads[1].resolve(voice); await load;
assert.match($('#voice-setup-detail').textContent, /首次失败：IMS 回读不一致/);
assert.match($('#voice-setup-detail').textContent, /重启通信：LIBUSB_ERROR_IO/);
assert.match($('#voice-setup-detail').textContent, /正在验证回滚后的配置/);
assert.equal($('#initialize-voice-module').disabled, true);
load = context.loadVoiceSetup();
context.activeDeviceID = 'giffgaff';
reads[2].resolve({ ...pending, summary: '过期的 5ber1 状态' }); reads[3].resolve(voice); await load;
assert.equal($('#voice-setup-summary').textContent, '等待重连 · 语音运行时已校验');
assert.equal(context.voiceSetupInFlight, false);
load = context.loadVoiceSetup();
reads[4].resolve({ ...pending, state: 'failed', summary: '需要重试', requires_adb_passcode: true }); reads[5].resolve(voice); await load;
assert.equal($('#initialize-voice-module').disabled, false);
assert.equal($('#initialize-voice-module').textContent, '自动解锁 ADB 并完成配置');
console.log('Module setup: original/reboot errors, pending rollback and stale module responses passed');
