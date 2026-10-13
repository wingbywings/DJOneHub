import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('../internal/relayadmin/web/app.js', import.meta.url), 'utf8');
const container = { children: [], replaceChildren() { this.children = []; }, append(...items) { this.children.push(...items); } };
const calls = [], prompts = [], messages = [];
let accept = false;
const element = (tag, textContent) => ({ tag, textContent, children: [], append(...items) { this.children.push(...items); } });
const context = vm.createContext({
  changed() { return true; }, $(selector) { assert.equal(selector, '#identities'); return container; }, element,
  signalLabel() { return 'WSS 未连接'; }, date() { return '未知'; }, copyButton: element,
  button(label, work) { return { label, work }; }, encodeURIComponent,
  async confirmAction(message) { prompts.push(message); return accept; },
  async api(path, body) { calls.push({ path, body }); return { deleted_count: 3 }; },
  notice(message) { messages.push(message); },
});
vm.runInContext(source.slice(source.indexOf('function renderIdentities('), source.indexOf('function renderAudit(')), context);
const identity = (id, revoked, kind) => ({ id, revoked, kind, hub_id: 'hub_1', modules: ['module-a'] });
context.renderIdentities([identity('hub_1', false, 'hub'), identity('ios_1', true, 'installation'), identity('hub_old', true, 'hub')]);
const operations = row => row.children[4].children;
assert.equal(operations(container.children[0]).filter(b => b.label === '删除记录').length, 0);
assert.equal(operations(container.children[0]).filter(b => b.label === '撤销').length, 1);
const phoneDelete = operations(container.children[1]).find(b => b.label === '删除记录');
await phoneDelete.work();
assert.equal(calls.length, 0); // cancelling must not send a deletion
accept = true;
await phoneDelete.work();
assert.equal(calls[0].path, 'identities/ios_1/delete');
assert.match(messages[0], /已删除 3 条/);
await operations(container.children[2]).find(b => b.label === '删除记录').work();
assert.equal(calls[1].path, 'identities/hub_old/delete');
assert.match(prompts.at(-1), /所属已失效 iPhone.*一并删除/);
assert.match(prompts.at(-1), /审计记录仍保留/);
assert.match(prompts.at(-1), /不会清理 Mac 或手机本机/);
console.log('Relay deletion UI: revoked-only buttons, cancellation, endpoint, result count and Hub cascade confirmation passed');

const requestContainer = { ...container, children: [] };
const requestContext = vm.createContext({
  changed() { return true; }, $(selector) { assert.equal(selector, '#requests'); return requestContainer; },
  element(tag, text) { return { ...element(tag, text), addEventListener() {} }; },
  document: { createTextNode(text) { return element('text', text); } },
  date() { return '未知'; }, states: {}, selections: new Map(), Set, encodeURIComponent,
  copyButton: element, async fingerprint() { return 'fixture-fingerprint'; },
  button(label, work) { return { label, work }; },
  async confirmAction(message) { prompts.push(message); return accept; },
  async api(path) { calls.push({ path }); return { deleted_authorizations: 2, cleared_count: 300 }; },
  notice(message) { messages.push(message); },
});
vm.runInContext(source.slice(source.indexOf('function renderRequests('), source.indexOf('async function fingerprint(')), requestContext);
vm.runInContext(source.slice(source.indexOf('async function clearAudit('), source.indexOf('async function refresh(')), requestContext);
requestContext.renderRequests([
  { state: 'approved', request: { id: 'request_1', name: 'Mac', modules: [] }, authorization: { id: 'grant_1', state: 'pending' } },
  { state: 'completed', request: { id: 'request_2', name: 'Mac 2', modules: [] } },
]);
const requestActions = row => row.children.at(-1).children;
const authorizationDelete = requestActions(requestContainer.children[0]).find(b => b.label === '删除此授权');
accept = false;
const before = calls.length;
await authorizationDelete.work();
assert.equal(calls.length, before);
accept = true;
await authorizationDelete.work();
assert.equal(calls.at(-1).path, 'requests/request_1/authorizations/grant_1/delete');
await requestActions(requestContainer.children[0]).find(b => b.label === '删除申请及授权').work();
assert.equal(calls.at(-1).path, 'requests/request_1/delete');
assert.match(messages.at(-1), /2 条授权/);
assert.equal(requestActions(requestContainer.children[1]).filter(b => b.label === '删除此授权').length, 0);
assert.equal(requestActions(requestContainer.children[1]).filter(b => b.label === '删除申请及授权').length, 1);
accept = false;
const beforeClear = calls.length;
await requestContext.clearAudit();
assert.equal(calls.length, beforeClear);
accept = true;
await requestContext.clearAudit();
assert.equal(calls.at(-1).path, 'audit/clear');
assert.match(prompts.at(-1), /待核对.*保留/);
assert.match(messages.at(-1), /300 条/);
console.log('Request/authorization deletion and audit clearing: cancellation, endpoints, counters and preservation notices passed');
