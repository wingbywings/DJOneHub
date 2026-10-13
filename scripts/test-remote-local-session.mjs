import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
const source = fs.readFileSync(new URL('../cmd/djonehub-macos/web/remote/app.js', import.meta.url),'utf8');
const html = fs.readFileSync(new URL('../cmd/djonehub-macos/web/remote/index.html', import.meta.url),'utf8');
assert.doesNotMatch(html, /type="password"|login-form|password-form|id="logout"/);
const nodes = new Map(), calls = [];
const response = (status, data) => ({ status, ok:status>=200 && status<300, async json(){return data;} });
let bootCount = 0, businessCount = 0, businessFailure = false;
const context = vm.createContext({
  base:'/api/remote-management', csrf:'old-csrf', authenticated:false, phones:[], phonesProfile:'',localSessionPromise:null,
  $(id){if (!nodes.has(id)) nodes.set(id,{hidden:false}); return nodes.get(id);}, clearDelivery(){}, error(code){return code;},
  async fetch(path, options){
    calls.push({path,options});
    if (path.endsWith('/session/local')) { bootCount++; return response(200,{authenticated:true,csrf:'new-csrf'}); }
    businessCount++;
    if (businessFailure) return response(409,{error:'call_or_recovery_active'});
    return businessCount === 1 ? response(401,{error:'management_login_required'}) : response(200,{done:true});
  },
});
vm.runInContext(source.slice(source.indexOf('function showUnavailable('),source.indexOf('async function waitJob(')),context);
await Promise.all([context.ensureLocalSession(),context.ensureLocalSession()]);
assert.equal(bootCount,1); assert.equal(context.authenticated,true); assert.equal(nodes.get('#session-panel').hidden,true);
await context.api('/connection',{profile:'fixture'});
assert.equal(businessCount,2); assert.equal(bootCount,2);
assert.equal(calls.at(-1).options.headers['X-DJOneHub-CSRF'],'new-csrf');
const before=calls.length;businessFailure=true;
await assert.rejects(context.api('/connection',{profile:'fixture'}),/call_or_recovery_active/);
assert.equal(calls.length,before+1); // no retry after business/uncertain failures
console.log('Mac local UI: no password controls, bootstrap deduplication, guarded renewal and no business replay passed');
