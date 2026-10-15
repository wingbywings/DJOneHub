// Exercise pagination/search controls and delayed API replies without a modem.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('../cmd/djonehub-macos/web/app.js', import.meta.url), 'utf8');
const block = (start, end) => source.slice(source.indexOf(start), source.indexOf(end));
function makeNode() {
  return {
    value: '', textContent: '', children: [], attributes: {}, disabled: false,
    append(...items) { this.children.push(...items); },
    replaceChildren(...items) { this.children = items; this.textContent = ''; },
    setAttribute(key, value) { this.attributes[key] = value; },
    addEventListener(event, fn) { this[event] = fn; },
    click() { if (!this.disabled) this.clickHandler?.(); },
  };
}
const nodes = new Map();
function $(id) {
  if (id.endsWith(' audio')) return null;
  if (!nodes.has(id)) {
    const node = makeNode();
    node.addEventListener = (event, fn) => { node[event === 'click' ? 'clickHandler' : event] = fn; };
    nodes.set(id, node);
  }
  return nodes.get(id);
}
const reads = [];
const context = vm.createContext({
  $, URLSearchParams, activeDeviceID: 'A',
  document: { createElement: makeNode }, callDuration: () => '1 分钟',
  selectedVoiceAgentCallID: '', currentVoiceAgentCalls: [], renderedVoiceAgentCallsSignature: '',
  voiceAgentCallsInFlight: '', voiceAgentCallsRequest: 0, voiceAgentCallsPage: 1,
  voiceAgentCallsPageSize: 10, voiceAgentCallsKeyword: '', voiceAgentCallsTotal: 0, voiceAgentCallsTotalPages: 0,
  api(path) { return new Promise((resolve, reject) => reads.push({ path, resolve, reject })); },
});
vm.runInContext(block('function renderVoiceAgentCalls(', 'async function loadCalls('), context);
vm.runInContext(block('$("#refresh-voice-agent-calls").addEventListener', '$("#back-to-voice-agent-calls").addEventListener'), context);
const tick = () => new Promise(setImmediate);
const call = (id = 'record-1') => ({
  call_id: id, number: '13800138000', ended_at: '2026-10-15T10:01:00Z',
  messages: [{ role: 'caller', text: '快递将在明天送达' }, { role: 'ai', text: '好的，再见' }],
});
const result = (options = {}) => ({ calls: [call()], total: 23, page: 1, limit: 10, total_pages: 3, ...options });

const first = context.loadVoiceAgentCalls();
assert.match(reads[0].path, /page=1&limit=10/);
assert.equal($('#voice-agent-call-list').attributes['aria-busy'], 'true');
await context.loadVoiceAgentCalls();
assert.equal(reads.length, 1); // duplicate concurrent refreshes share the pending request
reads[0].resolve(result()); await first;
assert.equal($('#voice-agent-call-count').textContent, '23 通');
assert.equal($('#voice-agent-call-prev').disabled, true);
assert.equal($('#voice-agent-call-next').disabled, false);
assert.match($('#voice-agent-call-page-summary').textContent, /1–10/);

$('#voice-agent-call-next').click();
assert.match(reads[1].path, /page=2/);
reads[1].resolve(result({ page: 2 })); await tick();
assert.match($('#voice-agent-call-page-summary').textContent, /11–20/);

$('#voice-agent-call-search').value = '  快递  ';
$('#voice-agent-call-search-form').submit({ preventDefault() {} });
assert.match(reads[2].path, /page=1/);
assert.equal(new URLSearchParams(reads[2].path.split('?')[1]).get('q'), '快递');
reads[2].resolve(result({ total: 1, total_pages: 1 })); await tick();
assert.equal($('#voice-agent-call-count').textContent, '1 通匹配');
assert.equal($('#voice-agent-call-list').children[0].children[0].children[2].textContent, '快递将在明天送达');
assert.equal($('#voice-agent-call-next').disabled, true);

$('#voice-agent-call-page-size').value = '20';
$('#voice-agent-call-page-size').change();
assert.match(reads[3].path, /page=1&limit=20/);
reads[3].resolve(result({ limit: 20, total: 0, total_pages: 0, calls: [] })); await tick();
assert.match($('#voice-agent-call-list').textContent, /没有找到匹配/);
assert.equal($('#voice-agent-call-page-number').textContent, '第 0 / 0 页');
assert.equal($('#voice-agent-call-prev').disabled, true);
assert.equal($('#voice-agent-call-next').disabled, true);

$('#clear-voice-agent-call-search').click();
assert.equal($('#voice-agent-call-search').value, '');
reads[4].resolve(result({ total: 0, total_pages: 0, calls: [] })); await tick();
assert.equal($('#voice-agent-call-list').textContent, '暂无 AI 通话记录');

// A late search reply or error must not replace a newer search or its controls.
context.voiceAgentCallsKeyword = 'old';
const old = context.loadVoiceAgentCalls();
context.voiceAgentCallsKeyword = 'new';
const latest = context.loadVoiceAgentCalls();
reads[6].resolve(result({ calls: [call('new')], total: 1, total_pages: 1 })); await latest;
reads[5].reject(new Error('late failure')); await old;
assert.equal(context.currentVoiceAgentCalls[0].call_id, 'new');
assert.equal($('#voice-agent-call-count').textContent, '1 通匹配');

context.voiceAgentCallsKeyword = 'device A';
const deviceA = context.loadVoiceAgentCalls();
context.activeDeviceID = 'B';
const deviceB = context.loadVoiceAgentCalls();
reads[8].resolve(result({ calls: [call('B')], total: 1, total_pages: 1 })); await deviceB;
reads[7].resolve(result({ calls: [call('A')] })); await deviceA;
assert.equal(context.currentVoiceAgentCalls[0].call_id, 'B');

const failed = context.loadVoiceAgentCalls();
reads[9].reject(new Error('offline')); await failed;
assert.match($('#voice-agent-call-list').textContent, /刷新记录/);
assert.equal($('#refresh-voice-agent-calls').disabled, false);
assert.equal(context.currentVoiceAgentCalls.length, 0);
const retry = context.loadVoiceAgentCalls();
reads[10].resolve(result({ calls: [call('recovered')], total: 1, total_pages: 1 })); await retry;
assert.equal(context.currentVoiceAgentCalls[0].call_id, 'recovered');

console.log('AI call records: paging, keyword search, previews, empty states, stale replies, device changes and retries passed');
