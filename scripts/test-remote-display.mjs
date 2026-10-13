// Exercise actual UI formatting: missing/stale observations must stay unknown.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const read = path => fs.readFileSync(new URL(path, import.meta.url), 'utf8');
const mac = read('../cmd/djonehub-macos/web/remote/app.js');
const admin = read('../internal/relayadmin/web/app.js');
function block(source, start, end) { return source.slice(source.indexOf(start), source.indexOf(end)); }
const node = (tag, text) => ({ tag, textContent:text, children:[], append(...items){this.children.push(...items);}, replaceChildren(...items){this.children=items;} });
const health = node('div');
const context = vm.createContext({ Date, Number, element:node, $(id){assert.equal(id,'#runtime-health'); return health;}, date(value){return value || '未知';} });
vm.runInContext(block(admin,'function signalLabel(', 'function renderRequests('), context);
const fresh = { signal_received_at:Date.now(), signal_state:'online', signal_observed_at:new Date().toISOString() };
assert.equal(context.signalLabel(fresh),'WSS 在线');
assert.equal(context.signalLabel({...fresh,signal_state:'offline'}),'WSS 未连接');
assert.equal(context.signalLabel({signal_state:'online'}),'WSS 在线状态未知');
assert.equal(context.signalLabel({...fresh,signal_received_at:Date.now()-13000}),'WSS 在线状态未知');
assert.equal(context.signalLabel({...fresh,signal_observed_at:new Date(Date.now()-3600000).toISOString()}),'WSS 在线'); // server/browser clock skew is not staleness
const text = n => [n.textContent || '', ...n.children.map(text)].join('\n');
context.renderHealth({state:'stale',snapshot:{observed_at:'old'}});
assert.match(text(health),/已过期/); assert.doesNotMatch(text(health),/查询正常/);
context.renderHealth({state:'fresh',snapshot:{running:true,identity_healthy:true,online_count:2,turn:{loaded:true},apns:{loaded:true,environment:'sandbox'}}});
assert.match(text(health),/查询正常/); assert.match(text(health),/尚无请求结果/); assert.doesNotMatch(text(health),/端到端正常|手机已收到|媒体已连通/);
const macContext=vm.createContext({Date,Number,date(value){return value || '未知';}});
vm.runInContext(block(mac,'function signalLabel(', 'async function refreshPresence('),macContext);
assert.equal(macContext.signalLabel({signal_state:'online'}),'WSS 在线状态未知');
assert.match(macContext.phonePresence({...fresh,push_registered:false}),/WSS 在线.*未登记推送/);
assert.match(macContext.phonePresence({signal_state:'offline',signal_received_at:Date.now(),signal_observed_at:new Date().toISOString(),push_registered:true,push_environment:'production'}),/WSS 未连接.*已登记 production 推送/);
macContext.node = node;
vm.runInContext(block(mac,'function phoneOptions(', 'async function readRoutes('),macContext);
const choices=node('select');
macContext.phoneOptions(choices,[{id:'phone_A',name:'测试手机',signal_state:'online',signal_observed_at:new Date(Date.now()-3600000).toISOString()}],'');
assert.match(choices.children[1].textContent,/WSS 在线 ·/); assert.doesNotMatch(choices.children[1].textContent,/未知/);
const main = read('../cmd/djonehub-macos/web/app.js');
const fields = new Map();
const storageWrites = [];
const selectionContext = vm.createContext({ requestedDeviceID:'module_B', activeDeviceID:'module_B', knownDevices:[],
  $(id){if(!fields.has(id)) fields.set(id,{...node('select'),value:''}); return fields.get(id);}, document:{createElement:node},
  Option:function(text,value){return {textContent:text,value};}, deviceOptionLabel(value){return value.id;}, localStorage:{setItem(...value){storageWrites.push(value);},removeItem(value){storageWrites.push(value);}},
  async api(){return [{id:'module_A',state:'ready'},{id:'module_B',state:'ready'}];}, async refreshActiveDevice(){throw Error('unexpected refresh');}
});
vm.runInContext(block(main,'async function loadDevices(', 'async function refreshActiveDevice('),selectionContext);
await selectionContext.loadDevices({refreshOnChange:false});
assert.equal(selectionContext.activeDeviceID,'module_B'); assert.equal(fields.get('#device-select').value,'module_B');
selectionContext.requestedDeviceID=selectionContext.activeDeviceID='missing_module';
await selectionContext.loadDevices({refreshOnChange:false});
assert.equal(selectionContext.activeDeviceID,'missing_module'); assert.equal(storageWrites.length,0);
selectionContext.api=async()=>{throw Error('offline');};
await selectionContext.loadDevices({refreshOnChange:false});
assert.equal(selectionContext.activeDeviceID,'missing_module'); assert.equal(storageWrites.length,0);
console.log('Display: live/offline/unknown, stale health and push/connection separation passed');
const linkContext = vm.createContext({URL});
vm.runInContext(block(mac, 'function managementLink(', '\nconst '), linkContext);
assert.equal(linkContext.managementLink('https://oops.work/relay-admin/'), 'https://oops.work/relay-admin/');
assert.equal(linkContext.managementLink('https://oops.work/relay-admin'), 'https://oops.work/relay-admin/');
assert.equal(linkContext.managementLink('https://admin.test'), 'https://admin.test/admin/');
console.log('Management links preserve configured paths and legacy origins');
const calls = [];
const adminPathContext = vm.createContext({adminBase:'/relay-admin/', csrf:'test-csrf', errorMessages:{}, showLogin(){throw Error('unexpected login');}, async fetch(path,options){calls.push({path,options}); return {ok:true,async json(){return {ok:true};},async blob(){return 'test-download';}};}});
vm.runInContext(block(admin,'async function api(', 'async function confirmAction('),adminPathContext);
await adminPathContext.api('session/status');
await adminPathContext.api('requests/test/authorization',{},true);
assert.equal(calls[0].path,'/relay-admin/api/session/status');
assert.equal(calls[1].path,'/relay-admin/api/requests/test/authorization');
assert.equal(calls[1].options.method,'POST');
assert.equal(calls[1].options.credentials,'same-origin');
console.log('Admin UI session and download requests use /relay-admin/api/');
