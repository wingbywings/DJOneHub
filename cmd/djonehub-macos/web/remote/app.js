function managementLink(value) { const u = new URL(value); u.pathname = u.pathname === '/' ? '/admin/' : u.pathname.replace(/\/$/, '') + '/'; return u.href; }
const $ = selector => document.querySelector(selector);
const base = '/api/remote-management';
let csrf = '', authenticated = false, busy = false, localSessionPromise = null;
let snapshot = { profiles: [], modules: [], active_profile: '' };
let presenceLoading = false;
let phones = [], phonesProfile = '', delivery = null, deliveryProfile = '', pairedIdentity = '', invitationState = '';
const signatures = new Map();
const messages = { management_login_required: '本机管理会话不可用，请重新连接页面', local_session_unavailable: '本机服务暂不可用，请核对服务已启动后重试', csrf_required: '本机管理会话已变化，请重新连接页面', activation_denied: '启动器票据已过期或使用过，请重新从启动器打开', management_busy: '另一项管理操作仍在进行，请刷新核对', call_or_recovery_active: '存在来电、通话或未完成恢复，请结束或处理后重试', scope_denied: '模块或手机不在当前有效授权范围内，请重新读取并选择', profile_not_found: '本机未找到此配置', profile_exists: '此钥匙串配置已经存在，不会覆盖', credential_recovery_required: '凭据事务结果不确定，请刷新核对；不要重新导入旧授权，需用新申请重新登记', keychain_unavailable: '钥匙串访问被拒绝或暂不可用，请允许当前程序访问后重新读取状态', platform_unsupported: '此操作需要 macOS 的钥匙串或远程音频发行版', invalid_management_state: '文件、配置或当前状态不符合操作要求', relay_request_failed: 'Relay 请求未成功；请核对服务版本、网络、授权和提交接口是否启用', management_storage_failed: '本机管理数据写入失败，请核对存储状态', operation_interrupted_check_state: '操作被中断，远端可能已消费授权，请先刷新核对结果', login_denied: '账号或密码不正确', setup_denied: '初始化凭据已过期或使用过', login_rate_limited: '尝试过于频繁，请稍后再试', same_origin_json_required: '请从本机的 127.0.0.1 管理地址打开页面', invalid_management_host: '请使用本机 127.0.0.1 的管理地址', job_not_found: '任务记录已过期，请刷新对象状态核对' };
const phases = { creating: '创建结果待核对', request_pending: '等待审批与授权导入', enrolling: '登记事务待恢复', ready: '已登记', refresh_pending: '凭据刷新结果待恢复' };
function notice(value) {
  $('#notice').textContent = value;
  const current = document.querySelector('.feature-dialog[open] .dialog-notice');
  if (current) current.textContent = value;
}
function openFeature(id) {
  if (!authenticated) return;
  const dialog = document.getElementById(id);
  document.querySelectorAll('.feature-dialog[open]').forEach(item => item.close());
  dialog.querySelector('.dialog-notice').textContent = '';
  dialog.showModal();
  document.body.classList.add('modal-open');
}
for (const trigger of document.querySelectorAll('[data-dialog]')) {
  trigger.addEventListener('click', () => openFeature(trigger.dataset.dialog));
}
for (const dialog of document.querySelectorAll('.feature-dialog')) {
  dialog.querySelector('[data-close-dialog]').addEventListener('click', () => dialog.close());
  dialog.addEventListener('close', () => {
    if (!document.querySelector('.feature-dialog[open]')) document.body.classList.remove('modal-open');
  });
  // Only a complete click outside the dialog dismisses it; dragging from a field does not.
  const outside = event => {
    const rect = dialog.getBoundingClientRect();
    return event.target === dialog && (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom);
  };
  let startedOutside = false;
  dialog.addEventListener('pointerdown', event => { startedOutside = outside(event); });
  dialog.addEventListener('click', event => { if (startedOutside && outside(event)) dialog.close(); startedOutside = false; });
}
function node(tag, text, className) { const n = document.createElement(tag); if (text !== undefined) n.textContent = text; if (className) n.className = className; return n; }
function date(value) { return value ? new Date(value).toLocaleString() : '旧记录 / 未知'; }
function error(code) { return messages[code] || code || '操作未完成'; }
function entry(id) { return snapshot.profiles.find(item => item.profile === id); }
function moduleLabel(id) { const item = snapshot.modules.find(module => module.id === id); return `${item?.alias || '历史授权模块'} · ${id}${item?.state !== 'ready' ? '（离线或不可用）' : ''}`; }
function changed(key, value) { const data = JSON.stringify(value); if (signatures.get(key) === data) return false; signatures.set(key, data); return true; }
function clearDelivery() { delivery = null; pairedIdentity = ''; invitationState = ''; $('#pairing-qr').removeAttribute('src'); $('#pairing-qr').hidden = true; $('#pairing-delivery').hidden = true; }
function showUnavailable() { document.querySelectorAll('dialog[open]').forEach(dialog => dialog.close()); authenticated = false; csrf = ''; phones = []; phonesProfile = ''; clearDelivery(); $('#management').hidden = true; $('#session-panel').hidden = false; }
function showManagement() { $('#session-panel').hidden = true; $('#management').hidden = false; }
async function ensureLocalSession() {
  if (localSessionPromise) return localSessionPromise;
  localSessionPromise = (async () => {
    const response = await fetch(base + '/session/local', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: '{}' });
    const data = await response.json().catch(() => ({}));
    if (!response.ok || !data.authenticated || !data.csrf) throw new Error(error(data.error || 'local_session_unavailable'));
    csrf = data.csrf; authenticated = true; showManagement(); return data;
  })();
  try { return await localSessionPromise; } finally { localSessionPromise = null; }
}
async function api(path, body) {
  const options = { credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-DJOneHub-CSRF': csrf } };
  if (body !== undefined) { options.method = 'POST'; options.body = JSON.stringify(body); }
  let response = await fetch(base + path, options), data = await response.json().catch(() => ({}));
  // These failures occur before the handler runs, so one retry cannot repeat a
  // successful mutation. Never retry uncertain network or business failures.
  if (response.status === 401 || response.status === 403 && data.error === 'csrf_required') {
    await ensureLocalSession(); options.headers['X-DJOneHub-CSRF'] = csrf;
    response = await fetch(base + path, options); data = await response.json().catch(() => ({}));
  }
  if (!response.ok) { if (response.status === 401) showUnavailable(); throw new Error(error(data.error)); }
  return data;
}
async function waitJob(id, silent = false) { const deadline = Date.now() + 125000; while (authenticated && Date.now() < deadline) { const j = await api('/jobs/' + encodeURIComponent(id)); if (j.state !== 'running') { if (j.state === 'failed') throw new Error(error(j.error)); if (!silent) notice('操作已完成，请核对对应状态。'); return j.result; } if (!silent) notice(`操作处理中（${Math.floor((Date.now() - new Date(j.started_at).getTime()) / 1000)} 秒）。页面可刷新核对，请勿重复导入授权。`); await new Promise(resolve => setTimeout(resolve, 450)); } throw new Error('操作仍可能在服务端处理，请刷新核对状态；不要重复导入旧授权。'); }
async function job(path, body, silent = false) { const j = await api(path, body); return waitJob(j.id, silent); }
async function action(button, work) { if (busy) return; busy = true; button.disabled = true; try { await work(); } catch (e) { notice(e.message); } finally { busy = false; if (button.isConnected) button.disabled = button.id === 'pairing-cancel' && invitationState !== 'pending'; if (authenticated) await refresh().catch(e => notice(e.message)); } }
function button(label, work, danger = false) { const b = node('button', label, danger ? 'danger' : 'secondary'); b.type = 'button'; b.addEventListener('click', () => action(b, work)); return b; }
async function confirmAction(text) {
  const dialog = node('dialog', undefined, 'confirmation');
  dialog.setAttribute('role', 'alertdialog');
  dialog.setAttribute('aria-labelledby', 'confirmation-title');
  dialog.append(Object.assign(node('h2', '确认管理操作'), { id: 'confirmation-title' }), node('p', text));
  const accept = node('button', '确认操作'), cancel = node('button', '取消', 'secondary');
  accept.type = cancel.type = 'button';
  const actions = node('div', undefined, 'actions');
  actions.append(cancel, accept); dialog.append(actions); document.body.append(dialog);
  return new Promise(resolve => {
    dialog.addEventListener('close', () => { const confirmed = dialog.returnValue === 'confirmed'; dialog.remove(); resolve(confirmed); }, { once: true });
    accept.addEventListener('click', () => dialog.close('confirmed'));
    cancel.addEventListener('click', () => dialog.close());
    dialog.showModal(); cancel.focus();
  });
}
function download(name, value) { const blob = new Blob([JSON.stringify(value, null, 2)], { type: 'application/json' }); const link = node('a'); const url = URL.createObjectURL(blob); link.href = url; link.download = name; document.body.append(link); link.click(); link.remove(); setTimeout(() => URL.revokeObjectURL(url), 1200); }
function copyButton(label, value) { const b = button(label, async () => { try { await navigator.clipboard.writeText(value); notice('已复制'); } catch { notice('浏览器未允许自动复制，请选择下方文本手动复制。'); const field = node('textarea'); field.value = value; field.readOnly = true; (document.querySelector('.feature-dialog[open] .dialog-notice') || $('#notice')).append(field); field.focus(); field.select(); } }); b.disabled = !value; return b; }
function link(label, href, external = false) { const a = node('a', label, 'secondary'); a.href = href; if (external) { a.target = '_blank'; a.rel = 'noopener noreferrer'; } return a; }
function signalLabel(item) { const age = Date.now() - item.signal_received_at; if (!item.signal_observed_at || !Number.isFinite(new Date(item.signal_observed_at).getTime()) || !Number.isFinite(age) || age < -1000 || age > 12000) return 'WSS 在线状态未知'; return item.signal_state === 'online' ? 'WSS 在线' : item.signal_state === 'offline' ? 'WSS 未连接' : 'WSS 在线状态未知'; }
function phonePresence(item) { return `${signalLabel(item)} · ${item.push_registered ? `已登记 ${item.push_environment} 推送` : '未登记推送'} · 状态采样：${date(item.signal_observed_at)}`; }
async function refreshPresence() { if (!authenticated || presenceLoading || !phonesProfile || document.hidden) return; const profile = phonesProfile; presenceLoading = true; try { const data = await api(`/profiles/${encodeURIComponent(profile)}/presence`); if (!authenticated || profile !== phonesProfile) return; const byID = new Map((data.state === 'fresh' ? data.installations || [] : []).map(item => [item.id, item])); for (const item of phones) { const observed = byID.get(item.id); item.signal_state = observed?.signal_state || 'unknown'; item.signal_observed_at = observed?.signal_observed_at; item.signal_received_at = observed ? Date.now() : undefined; item.revoked = observed?.revoked ?? item.revoked; } $('#phones').querySelectorAll('[data-presence-id]').forEach(row => { const item = phones.find(phone => phone.id === row.dataset.presenceId); if (item) row.textContent = phonePresence(item); }); $('#phones').querySelectorAll('[data-identity-state-id]').forEach(row => { const item = phones.find(phone => phone.id === row.dataset.identityStateId); if (item) row.textContent = `${item.name || '未命名 iPhone'} · ${item.revoked ? '已撤销' : '授权有效'}`; }); $('#presence-note').textContent = data.state === 'fresh' ? '每五秒更新连接观测；WSS 未连接仍可能通过已登记的后台推送收到来电。' : data.state === 'expired' ? '查询凭据已到期，请点击“读取手机列表”重新授权；不会自动读取钥匙串。' : '在线观测暂不可用或尚未开始；已开始的查询会自动重试，也可重新读取手机列表。'; } catch { if (profile === phonesProfile) { $('#presence-note').textContent = '在线观测暂不可用，请重新读取手机列表。'; $('#phones').querySelectorAll('[data-presence-id]').forEach(row => { row.textContent = 'WSS 在线状态未知；在线观测暂不可用'; }); } } finally { presenceLoading = false; } }
function selectedModules(id) { return Array.from($(id).querySelectorAll('input:checked')).map(input => input.value); }
function renderModules(id, modules) { const box = $(id); const selected = new Set(selectedModules(id)); box.querySelectorAll('label,p').forEach(n => n.remove()); for (const item of modules) { const label = node('label', undefined, 'check'), input = node('input'); input.type = 'checkbox'; input.value = item.id; input.checked = selected.has(item.id); const text = node('span', item.alias || '未命名模块'); text.append(node('small', item.id), node('small', item.state === 'ready' ? '模块可用' : '模块离线；授权不代表音频已就绪')); label.append(input, text); box.append(label); } if (!modules.length) box.append(node('p', '没有可选择的模块，请连接设备或核对配置授权范围。')); }
function fillProfiles(id) { const select = $(id), previous = select.value; select.replaceChildren(node('option', '请选择已登记配置')); select.firstChild.value = ''; for (const item of snapshot.profiles.filter(item => item.status.phase === 'ready')) { const option = node('option', `${item.label} · ${item.status.relay_url}`); option.value = item.profile; select.append(option); } select.value = entry(previous)?.status.phase === 'ready' ? previous : snapshot.active_profile; }
function renderPairingModules() { const selected = entry($('#pairing-profile').value); const scope = selected?.status.principal?.modules || []; renderModules('#pairing-modules', snapshot.modules.filter(module => scope.includes(module.id))); }
function renderProfiles() {
  const container = $('#profiles'); container.replaceChildren();
  for (const item of snapshot.profiles) {
    const p = node('div', undefined, 'profile'); p.append(node('h3', `${item.label} · ${phases[item.status.phase] || item.status.phase}${item.profile === snapshot.active_profile ? ' · 当前连接' : ''}`), node('p', `服务器：${item.status.relay_url}`), node('p', `profile：${item.profile}`, 'mono'), node('p', '公钥 SHA-256：' + (item.status.public_key_fingerprint || '待核对'), 'mono'));
    const scope = item.status.principal?.modules || item.request?.modules?.map(module => module.id) || []; for (const id of scope) p.append(node('small', moduleLabel(id)));
    if (item.error) p.append(node('p', error(item.error)));
    if (item.request) p.append(node('p', `申请：${item.request.id} · 有效期至 ${date(item.request.expires_at)}`, 'mono'));
    const controls = node('div', undefined, 'actions');
    const hubID = item.status.principal?.id;
    if (hubID) { p.append(node('p', 'Hub ID：' + hubID, 'mono')); controls.append(copyButton('复制 Hub ID', hubID)); }
    controls.append(copyButton('复制公钥指纹', item.status.public_key_fingerprint || ''));
    if (item.request) controls.append(copyButton('复制申请编号', item.request.id));
    if (item.admin_url) controls.append(link('打开 Relay 授权管理', managementLink(item.admin_url), true));
    const adminLabel = node('label', 'Relay 管理地址（可选）'), adminInput = node('input'); adminInput.type = 'url'; adminInput.value = item.admin_url || ''; adminInput.placeholder = 'https://oops.work/relay-admin/'; adminLabel.append(adminInput); p.append(adminLabel);
    controls.append(button('保存管理地址', async () => { await job(`/profiles/${encodeURIComponent(item.profile)}/actions`, { action: 'admin_url', admin_url: adminInput.value.trim() }); notice('管理地址已保存'); }));
    controls.append(button('重新读取状态', async () => { await job(`/profiles/${encodeURIComponent(item.profile)}/actions`, { action: 'refresh' }); notice('本机登记状态已更新；登记不等于信令在线。'); }));
    const label = node('label', '显示备注'), input = node('input'); input.value = item.label; input.maxLength = 128; label.append(input); p.append(label);
    controls.append(button('保存备注', async () => { await job(`/profiles/${encodeURIComponent(item.profile)}/actions`, { action: 'rename', name: input.value }); notice('备注已保存'); }));
    if (item.status.phase === 'request_pending' && item.request) {
      controls.append(button('提交到 Relay', async () => { await job(`/profiles/${encodeURIComponent(item.profile)}/actions`, { action: 'submit' }); notice('申请已提交，请在 Relay 管理页审核。'); }), button('下载公有申请文件', async () => { download('djonehub-registration-request.json', item.request); notice('已下载公有申请，可在 Relay 管理页导入。'); }));
      const fileLabel = node('label', '导入该申请的授权文件'), file = node('input'); file.type = 'file'; file.accept = 'application/json,.json'; fileLabel.append(file); p.append(fileLabel);
      file.addEventListener('change', () => { const selected = file.files[0]; if (!selected) return; action(file, async () => { if (selected.size > 16384) throw new Error('授权文件超过 16 KiB'); const authorization = JSON.parse(await selected.text()); if (authorization.version !== 2 || authorization.request_id !== item.request.id || authorization.relay_url !== item.status.relay_url || authorization.public_key_fingerprint !== item.status.public_key_fingerprint) throw new Error('授权文件不属于此申请、服务器或公钥，请选择对应文件。'); const modules = authorization.grant?.modules || []; if (!modules.length || modules.some(id => !item.request.modules.some(module => module.id === id))) throw new Error('授权模块不属于此申请'); if (!await confirmAction(`将此授权登记到钥匙串？\n申请：${authorization.request_id}\n服务器：${authorization.relay_url}\n模块：${modules.map(moduleLabel).join('\n')}\n到期：${date(authorization.grant.expires_at)}\n授权只能消费一次；结果不确定时需先核对状态。`)) return; await job(`/profiles/${encodeURIComponent(item.profile)}/actions`, { action: 'enroll', authorization }); notice('Mac 登记完成，可以连接此配置并配对手机。请删除授权交付文件。'); }).finally(() => { file.value = ''; }); });
    }
    if (item.profile !== snapshot.active_profile) controls.append(button('删除本机配置', async () => { if (!await confirmAction(`删除 ${item.label} 的本机钥匙串记录和列表项目？\n这不会撤销 Relay 上的 Mac 或手机身份。若需取消远程权限，请在 Relay 管理页撤销。已有模块目标不会自动改成其他手机。`)) return; await job(`/profiles/${encodeURIComponent(item.profile)}/actions`, { action: 'remove' }); notice('本机配置已删除。远端身份仍需在 Relay 管理页管理。'); }, true));
    p.append(controls); container.append(p);
  }
  if (!snapshot.profiles.length) container.append(node('p', '暂无本机配置。创建新申请，或导入已存在的旧钥匙串配置。'));
}
async function refresh() {
  if (!authenticated) return;
  const data = await api('/profiles');
  if (!authenticated) return;
  snapshot = data;
  $('#overview').textContent = `当前连接：${entry(data.active_profile)?.label || '未连接'} · 信令：${data.connection_state} · 重启选择：${entry(data.autostart_profile)?.label || '无'}${data.native_control_configured ? '' : ' · 尚未启用原生音频控制，请用发行启动器运行'}`;
  $('#connection-dot').classList.toggle('online', data.connection_state === 'online');
  $('#module-count').textContent = data.modules.filter(item => item.state === 'ready').length;
  $('#profile-count').textContent = data.profiles.filter(item => item.status.phase === 'ready').length;
  $('#request-count').textContent = data.profiles.filter(item => item.status.phase !== 'ready').length;
  const pending = $('#pending-operation'); pending.replaceChildren();
  if (data.pending_job) pending.append(button('查看正在进行的操作', async () => { await waitJob(data.pending_job.id); notice('操作已完成，请核对对应对象。'); }));
  if (changed('profiles', [data.profiles, data.active_profile])) {
    renderProfiles(); fillProfiles('#connection-profile'); fillProfiles('#pairing-profile');
    fillProfiles('#phones-profile'); fillProfiles('#invitations-profile');
    if (phonesProfile && phonesProfile !== $('#phones-profile').value) clearPhones();
    if (invitationsProfile && invitationsProfile !== $('#invitations-profile').value) clearInvitations();
    renderPairingModules();
  }
  if (changed('modules', data.modules.map(({ id, alias, state }) => ({ id, alias, state })))) {
    renderModules('#registration-modules', data.modules.filter(module => module.state === 'ready'));
    renderPairingModules();
  }
  if (routesProfile && routesProfile !== data.active_profile) {
    routesProfile = ''; $('#module-routes').replaceChildren(node('p', '当前连接已变化，请重新读取模块配置。'));
  }
}
let invitationsProfile = '', routesProfile = '';
function clearPhones() {
  phones = []; phonesProfile = '';
  $('#phones').replaceChildren(node('p', '选择 Mac 配置后，点击“读取手机列表”。'));
  $('#presence-note').textContent = '读取列表后每五秒更新 WSS 连接观测；推送登记与当前连接分别显示。';
}
function clearInvitations() {
  invitationsProfile = '';
  $('#invitations').replaceChildren(node('p', '选择 Mac 配置后，点击“读取邀请列表”。'));
}
$('#phones-profile').addEventListener('change', clearPhones);
$('#invitations-profile').addEventListener('change', clearInvitations);
clearPhones(); clearInvitations();
$('#module-routes').append(node('p', '连接 Mac 配置后，点击“读取模块配置”。'));

async function readPhones() { const profile = $('#phones-profile').value; if (!profile) throw new Error('请选择已登记的 Mac 配置'); const result = await job('/installations', { profile, action: 'list' }); if ($('#phones-profile').value !== profile) return; phones = result.installations; phones.forEach(item => { item.signal_received_at = Date.now(); }); phonesProfile = profile; renderPhones(); notice('手机列表已更新；WSS 连接与后台推送分别显示。'); }
function renderPhones() { const box = $('#phones'); box.replaceChildren(); for (const item of phones) { const rowProfile = phonesProfile; const row = node('div', undefined, 'phone'); row.append(node('h3', `${item.name || '未命名 iPhone'} · ${item.revoked ? '已撤销' : '授权有效'}`), node('p', item.id, 'mono'), node('p', `配对：${date(item.created_at)} · 最近认证：${date(item.last_authenticated)}`), node('p', phonePresence(item))); row.firstChild.dataset.identityStateId = item.id; const presence = row.lastChild; presence.dataset.presenceId = item.id; row.append(copyButton('复制手机 ID', item.id), copyButton('复制公钥指纹', item.public_key_fingerprint)); for (const id of item.modules) row.append(node('small', moduleLabel(id))); if (!item.revoked) { const label = node('label', '手机备注'), input = node('input'); input.value = item.name || ''; input.maxLength = 128; label.append(input); row.append(label, button('保存手机备注', async () => { await job('/installations', { profile: rowProfile, action: 'rename', id: item.id, name: input.value }); await readPhones(); }), button('撤销此手机', async () => { const refs = await job('/installations', { profile: rowProfile, action: 'preview', id: item.id }); const text = refs.map(ref => `${ref.alias} · ${ref.module}${ref.active ? '（有通话）' : ''}${ref.ai_enabled ? '（AI 接听已启用）' : ''}`).join('\n'); if (!await confirmAction(`撤销 ${item.name || item.id}？\n受影响模块：\n${text || '无当前引用'}\n活动远程通话可能中断；不会自动改成其他手机。`)) return; await job('/installations', { profile: rowProfile, action: 'revoke', id: item.id }); await readPhones(); notice('手机授权已撤销。请重新选择相关模块的目标。'); }, true)); } box.append(row); } if (!phones.length) box.append(node('p', '暂无已配对手机。')); }
function phoneOptions(select, items, current) { select.replaceChildren(); const empty = node('option', '请选择有效 iPhone'); empty.value = ''; select.append(empty); for (const phone of items) { phone.signal_received_at = Date.now(); const option = node('option', `${phone.name || '未命名 iPhone'} · ${phone.id.slice(0, 12)}… · ${signalLabel(phone)} · ${phone.push_registered ? '已登记推送' : '未登记推送'}`); option.value = phone.id; select.append(option); } if (current && !items.some(item => item.id === current)) { const invalid = node('option', '原目标已失效 / 不属于此配置，请重新选择'); invalid.value = current; invalid.disabled = true; select.append(invalid); } select.value = current || ''; }
async function readRoutes() { const profile = snapshot.active_profile; if (!profile) throw new Error('请先连接已登记的 Mac 配置'); const owner = entry(profile); const result = await job('/installations', { profile, action: 'list' }); if (profile !== snapshot.active_profile) throw new Error('当前连接已变化，请重新读取模块配置'); routesProfile = profile; const box = $('#module-routes'); box.replaceChildren(); const scope = owner.status.principal.modules; for (const module of snapshot.modules.filter(module => scope.includes(module.id))) { const row = node('div', undefined, 'route'); row.append(node('h3', moduleLabel(module.id)), link('打开此模块的来电设置', '/?device=' + encodeURIComponent(module.id) + '#calls')); if (module.state !== 'ready') { row.append(node('p', '模块离线，当前不能保存接听设置。')); box.append(row); continue; } const response = await fetch(`/api/devices/${encodeURIComponent(module.id)}/remote-calls/status`); if (!response.ok) { row.append(node('p', '模块不可用，请刷新后重试')); box.append(row); continue; } const state = await response.json(); const config = state.config; const valid = result.installations.filter(phone => !phone.revoked && phone.hub_id === owner.status.principal.id && phone.modules.includes(module.id)); const form = node('form'), enabled = node('input'); enabled.type = 'checkbox'; enabled.checked = config.enabled; const enabledLabel = node('label', undefined, 'check'); enabledLabel.append(enabled, document.createTextNode('启用远程接听')); const phoneLabel = node('label', '目标 iPhone'), select = node('select'); phoneOptions(select, valid, config.target_installation_id); phoneLabel.append(select); const save = node('button', '保存此模块'); save.type = 'submit'; row.append(node('p', state.effective_route === 'agent' ? 'AI 当前已启用；保存远程目标后仍由 AI 接听。' : 'AI 未启用；远程接听按此模块设置触发。')); form.append(enabledLabel, phoneLabel, save); form.addEventListener('submit', event => { event.preventDefault(); action(save, async () => { if (enabled.checked && !valid.some(item => item.id === select.value)) throw new Error('请选择有效且覆盖此模块的 iPhone'); const candidate = { ...config, enabled: enabled.checked, target_installation_id: valid.some(item => item.id === select.value) ? select.value : '' }; if (snapshot.active_profile !== profile) throw new Error('当前连接已变化，请重新读取模块配置'); await job('/routes', { module: module.id, config: candidate }); notice('此模块设置已保存。AI 接听状态保持当前设置。'); }); }); row.append(form); box.append(row); } }
function renderDelivery(value, profile) { clearDelivery(); delivery = value; deliveryProfile = profile; invitationState = 'pending'; $('#pairing-delivery').hidden = false; $('#pairing-state').textContent = `邀请：${value.grant.id}，等待该邀请的手机登记结果`; $('#pairing-qr').hidden = !value.qr_image; if (value.qr_image) $('#pairing-qr').src = value.qr_image; else $('#pairing-state').textContent += '。二维码暂不可用，请减少此次模块数，或下载文件导入手机。'; $('#pairing-download').disabled = false; $('#pairing-cancel').disabled = false; $('#pairing-apply').hidden = true; updateExpiry(); }
function updateExpiry() { if (!delivery) return; const remaining = Math.max(0, Math.ceil((new Date(delivery.grant.expires_at).getTime() - Date.now()) / 1000)); $('#pairing-expiry').textContent = `有效期至 ${date(delivery.grant.expires_at)} · 剩余 ${remaining} 秒`; if (!remaining && invitationState === 'pending') { invitationState = 'expired'; $('#pairing-state').textContent = '邀请已过期，请重新生成。'; $('#pairing-qr').removeAttribute('src'); $('#pairing-qr').hidden = true; delivery.grant.secret = ''; $('#pairing-download').disabled = true; $('#pairing-cancel').disabled = true; } }
async function checkPairing(silent = false) { if (!delivery) throw new Error('没有可查询的邀请'); const id = delivery.grant.id, profile = deliveryProfile; const state = await job(`/pairings/${encodeURIComponent(id)}/actions`, { profile, action: 'status' }, silent); if (!delivery || delivery.grant.id !== id) return; invitationState = state.state; const labels = { pending: '等待扫码确认', consumed: '配对完成', revoked: '已撤销', expired: '已过期' }; $('#pairing-state').textContent = `${labels[state.state] || state.state}${state.identity_id ? ' · 本次实际登记：' + state.identity_id : ''}`; if (state.state !== 'pending') { $('#pairing-qr').removeAttribute('src'); $('#pairing-qr').hidden = true; delivery.grant.secret = ''; $('#pairing-download').disabled = true; $('#pairing-cancel').disabled = true; } if (state.state === 'consumed' && state.identity_id) { pairedIdentity = state.identity_id; $('#pairing-apply').hidden = false; } }
$('#session-retry').addEventListener('click', () => action($('#session-retry'), async () => { await ensureLocalSession(); await refresh(); notice('已连接本机管理'); }));
$('#refresh').addEventListener('click', () => action($('#refresh'), async () => { await refresh(); notice('管理状态已刷新'); }));
$('#registration-form').addEventListener('submit', event => { event.preventDefault(); action(event.submitter, async () => { const modules = selectedModules('#registration-modules'); if (!modules.length) throw new Error('请至少勾选一个本机可用模块'); await job('/enrollment-requests', { name: $('#registration-name').value, relay_url: $('#relay-url').value, modules, admin_url: $('#relay-admin-url').value.trim() }); notice('签名申请已保存在本机。请在配置列表提交到 Relay，或下载公有申请文件供管理员审核。'); }); });
$('#legacy-form').addEventListener('submit', event => { event.preventDefault(); action(event.submitter, async () => { await job('/profiles/import', { profile: $('#legacy-profile').value, name: $('#legacy-name').value }); notice('旧配置已加入管理列表，身份未被重新登记。'); }); });
$('#connection-form').addEventListener('submit', event => { event.preventDefault(); action(event.submitter, async () => { const profile = $('#connection-profile').value; if (!profile) throw new Error('请选择已登记的配置'); if (!await confirmAction(`连接 ${entry(profile)?.label}？\n旧信令连接将退出，现有模块目标将按新配置重新校验。活动通话或恢复未完成时会拒绝切换。`)) return; await job('/connection', { profile, auto_start: $('#auto-start').checked }); notice('连接任务已启动；信令在线状态见上方。'); }); });
for (const [id, clear] of [['#disconnect', false], ['#disconnect-clear', true]]) $(id).addEventListener('click', () => action($(id), async () => { if (!await confirmAction(clear ? '断开当前连接并取消下次启动自动连接？' : '断开当前连接？下次启动仍使用保存的配置。')) return; await job('/connection', { profile: '', auto_start: clear }); clearDelivery(); notice('当前连接已断开'); }));
$('#pairing-profile').addEventListener('change', renderPairingModules);
$('#pairing-form').addEventListener('submit', event => { event.preventDefault(); action(event.submitter, async () => { const profile = $('#pairing-profile').value, modules = selectedModules('#pairing-modules'); if (!profile || !modules.length) throw new Error('请选择 Mac 配置和至少一个已授权模块'); if (delivery && invitationState === 'pending') { if (!await confirmAction('撤销当前未使用邀请，并创建新邀请？')) return; await job(`/pairings/${encodeURIComponent(delivery.grant.id)}/actions`, { profile: deliveryProfile, action: 'revoke' }); } const result = await job('/pairings', { profile, name: $('#phone-name').value, modules }); renderDelivery(result, profile); notice('请用 iPhone App 扫码并核对服务器与模块授权。'); }); });
$('#pairing-download').addEventListener('click', () => { if (!delivery || invitationState !== 'pending') return; download('djonehub-phone-pairing.json', { version: 1, relay_url: delivery.relay_url, grant: delivery.grant }); notice('配对文件已下载，仅在有效期内可用。配对后删除交付文件。'); });
$('#pairing-check').addEventListener('click', () => action($('#pairing-check'), checkPairing));
$('#pairing-cancel').addEventListener('click', () => action($('#pairing-cancel'), async () => { if (!delivery || !await confirmAction('撤销这个尚未使用的邀请？已配对手机需从手机列表撤销。')) return; await job(`/pairings/${encodeURIComponent(delivery.grant.id)}/actions`, { profile: deliveryProfile, action: 'revoke' }); await checkPairing(); notice('邀请已撤销'); }));
$('#pairing-apply').addEventListener('click', () => action($('#pairing-apply'), async () => { if (!delivery || !pairedIdentity || snapshot.active_profile !== deliveryProfile) throw new Error('请先连接本次配对所属的 Mac 配置，再应用目标。'); const candidates = []; for (const id of delivery.grant.modules) { const response = await fetch(`/api/devices/${encodeURIComponent(id)}/remote-calls/status`); if (!response.ok) { candidates.push({ id, unavailable: true }); continue; } const state = await response.json(); candidates.push({ id, config: state.config, ai: state.effective_route === 'agent' }); } if (!await confirmAction(`把本次实际配对的 ${pairedIdentity} 应用到以下模块？\n${candidates.map(item => `${moduleLabel(item.id)}${item.unavailable ? '（当前不可用，将跳过）' : item.ai ? '（AI 已启用，保存后仍由 AI 接听）' : ''}`).join('\n')}\n逐项保存，成功项立即生效；失败项不会自动回滚，可随后重试。`)) return; const results = []; for (const item of candidates) { if (item.unavailable) { results.push(`${moduleLabel(item.id)}：不可用`); continue; } try { await job('/routes', { module: item.id, config: { ...item.config, enabled: true, target_installation_id: pairedIdentity } }); results.push(`${moduleLabel(item.id)}：已保存`); } catch (e) { results.push(`${moduleLabel(item.id)}：${e.message}`); } } notice(results.join('\n')); }));
$('#phones-refresh').addEventListener('click', () => action($('#phones-refresh'), readPhones));
$('#routes-refresh').addEventListener('click', () => action($('#routes-refresh'), readRoutes));
setInterval(() => { updateExpiry(); if (authenticated && !busy && !document.hidden) { if (delivery && invitationState === 'pending') void action($('#pairing-check'), () => checkPairing(true)); else void refresh().catch(e => notice(e.message)); } }, 5000);
(async () => { if (location.hash) history.replaceState(null, '', location.pathname); try { await ensureLocalSession(); await refresh(); } catch (e) { showUnavailable(); notice(e.message); } })();

async function readInvitations() { const profile = $('#invitations-profile').value; if (!profile) throw new Error('请选择已登记的 Mac 配置'); const result = await job('/pairings/query', { profile }); if (profile !== $('#invitations-profile').value) return; invitationsProfile = profile; const box = $('#invitations'); box.replaceChildren(); for (const item of result.pairings) { const row = node('div', undefined, 'phone'); const labels = { pending: '待扫码', consumed: '已消费', revoked: '已撤销', expired: '已过期' }; row.append(node('strong', `${item.name || '未命名邀请'} · ${labels[item.state] || item.state}`), node('p', item.id, 'mono'), node('p', `创建：${date(item.created_at)} · 到期：${date(item.expires_at)}`)); for (const module of item.modules) row.append(node('small', moduleLabel(module))); if (item.identity_id) row.append(node('p', '此邀请实际登记：' + item.identity_id, 'mono')); if (item.state === 'pending') row.append(button('撤销这个邀请', async () => { if (!await confirmAction(`撤销邀请 ${item.id}？对应的二维码和文件会失效。`)) return; await job(`/pairings/${encodeURIComponent(item.id)}/actions`, { profile, action: 'revoke' }); await readInvitations(); notice('邀请已撤销'); }, true)); box.append(row); } if (!result.pairings.length) box.append(node('p', '暂无已签发邀请')); }
$('#invitations-refresh').addEventListener('click', () => action($('#invitations-refresh'), readInvitations));

setInterval(() => { void refreshPresence(); }, 5000);
