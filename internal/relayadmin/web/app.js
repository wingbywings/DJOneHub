const $ = (selector) => document.querySelector(selector);
const adminBase = document.querySelector('meta[name="djonehub-admin-base"]').content;
let csrf = '';
let configured = false;
let authenticated = false;
let busy = false;
let loading = false;
let runtimeReceivedAt = 0, lastRuntime = null;
const selections = new Map();
const renderSignatures = new Map();
function changed(key, items) { const value = JSON.stringify(items); if (renderSignatures.get(key) === value) return false; renderSignatures.set(key, value); return true; }
const errorMessages = { management_login_required: '请重新登录', login_denied: '账号或密码不正确', setup_denied: '初始化凭据无效、已过期或已使用', login_rate_limited: '尝试过于频繁，请稍后再试', username_or_password_invalid: '账号格式或密码长度不符合要求', management_scope_denied: '没有此对象的管理权限', management_state_invalid: '当前状态不允许此操作，请刷新核对', management_capacity: '待处理授权数量已达上限', management_storage_unavailable: '管理数据暂不可用，请检查服务', csrf_required: '管理会话已变化，请重新登录', enrollment_origin_mismatch: '申请的 Relay 地址与当前服务器不一致', audit_unavailable_operation_not_started: '操作记录不可用，本次操作尚未执行', operation_result_uncertain_refresh_required: '操作结果记录未完成，请刷新对象状态核对', invalid_admin_request: '申请格式无效或文件过大' };
const actionNames = { import_request: '导入注册申请', approve_request: '批准注册申请', reject_request: '拒绝或撤销申请', issue_authorization: '签发授权文件', revoke_identity: '撤销设备身份', delete_revoked_identity: '删除已撤销设备记录', delete_enrollment_request: '删除注册申请', delete_enrollment_authorization: '删除授权文件', clear_audit: '清除已完成管理记录' };
const resultNames = { pending: '结果待核对', success: '成功', failed: '失败' };
const states = { pending: '待审核', approved: '已批准', rejected: '已拒绝', expired: '已过期', completed: '已注册' };
function notice(text) {
  $('#notice').textContent = text;
  const current = document.querySelector('.feature-dialog[open] .dialog-notice');
  if (current) current.textContent = text;
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
  // Dismiss only when both ends of the click are outside the dialog.
  const outside = event => {
    const rect = dialog.getBoundingClientRect();
    return event.target === dialog && (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom);
  };
  let startedOutside = false;
  dialog.addEventListener('pointerdown', event => { startedOutside = outside(event); });
  dialog.addEventListener('click', event => { if (startedOutside && outside(event)) dialog.close(); startedOutside = false; });
}
function element(tag, text, className) {
  const node = document.createElement(tag);
  if (text !== undefined) node.textContent = text;
  if (className) node.className = className;
  return node;
}
function date(value) { return value ? new Date(value).toLocaleString() : '旧记录 / 未知'; }
function showLogin() {
  document.querySelectorAll('dialog[open]').forEach(dialog => dialog.close());
  authenticated = false; csrf = ''; renderSignatures.clear(); selections.clear();
  $('#management').hidden = true; $('#login-panel').hidden = false; $('#logout').hidden = true;
  $('#setup-field').hidden = configured;
  $('#login-title').textContent = configured ? '管理员登录' : '初始化管理员';
  $('#login-button').textContent = configured ? '登录' : '创建管理员';
  $('#password').autocomplete = configured ? 'current-password' : 'new-password';
}
async function api(path, body, download = false) {
  const options = { credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-DJOneHub-CSRF': csrf } };
  if (body !== undefined) { options.method = 'POST'; options.body = JSON.stringify(body); }
  const response = await fetch(adminBase + 'api/' + path, options);
  if (!response.ok) {
    if (response.status === 401) showLogin();
    const result = await response.json().catch(() => ({}));
    throw new Error(errorMessages[result.error] || `请求失败（HTTP ${response.status}）`);
  }
  return download ? response.blob() : response.json();
}
async function confirmAction(message) {
  const dialog = element('dialog', undefined, 'confirmation');
  dialog.setAttribute('role', 'alertdialog');
  dialog.setAttribute('aria-labelledby', 'confirmation-title');
  dialog.append(Object.assign(element('h2', '确认管理操作'), { id: 'confirmation-title' }), element('p', message));
  const actions = element('div', undefined, 'actions');
  const accept = element('button', '确认操作'), cancel = element('button', '取消', 'secondary');
  accept.type = cancel.type = 'button';
  actions.append(cancel, accept); dialog.append(actions); document.body.append(dialog);
  return new Promise(resolve => {
    dialog.addEventListener('close', () => { const confirmed = dialog.returnValue === 'confirmed'; dialog.remove(); resolve(confirmed); }, { once: true });
    accept.addEventListener('click', () => dialog.close('confirmed'));
    cancel.addEventListener('click', () => dialog.close());
    dialog.showModal(); cancel.focus();
  });
}
async function action(button, work) {
  if (busy) return;
  busy = true; button.disabled = true;
  try { await work(); await refresh(); } catch (error) { notice(error.message); }
  finally { busy = false; if (button.isConnected) button.disabled = false; }
}
function button(label, work, danger = false) {
  const node = element('button', label, danger ? 'danger' : 'secondary');
  node.type = 'button';
  node.addEventListener('click', () => action(node, work));
  return node;
}
function copyButton(label, value) { const b = button(label, async () => { const text = typeof value === 'function' ? await value() : value; try { await navigator.clipboard.writeText(text); notice('已复制'); } catch { notice('浏览器未允许自动复制，请选择文本手动复制。'); const field = element('textarea'); field.value = text; field.readOnly = true; (document.querySelector('.feature-dialog[open] .dialog-notice') || $('#notice')).append(field); field.focus(); field.select(); } }); b.disabled = !value; return b; }
function signalLabel(item) { const age = Date.now() - item.signal_received_at; if (!item.signal_observed_at || !Number.isFinite(new Date(item.signal_observed_at).getTime()) || !Number.isFinite(age) || age < -1000 || age > 12000) return 'WSS 在线状态未知'; return item.signal_state === 'online' ? 'WSS 在线' : item.signal_state === 'offline' ? 'WSS 未连接' : 'WSS 在线状态未知'; }
function activityLabel(value, label) { if (!value?.loaded) return `${label}：未加载`; const result = value.last_result === 'accepted' ? '接受' : '失败'; return `${label}：已加载${value.environment ? '（' + value.environment + '）' : ''}；${value.last_attempt ? `最近请求${result} · ${date(value.last_attempt)}；成功 ${value.successes} / 失败 ${value.failures}` : '尚无请求结果'}`; }
function renderHealthSummary(runtime) {
  const fresh = runtime?.state === 'fresh' && runtime.snapshot;
  const running = fresh && runtime.snapshot.running;
  $('#runtime-dot').classList.toggle('online', Boolean(running));
  $('#runtime-state').textContent = fresh ? (running ? 'Relay 正在运行' : 'Relay 已停止') : runtime?.state === 'stale' ? '运行观测已过期' : '运行状态未知';
  $('#online-count').textContent = running ? runtime.snapshot.online_count : '—';
}
function renderHealth(runtime) { renderHealthSummary(runtime); const box = $('#runtime-health'); box.replaceChildren(); if (runtime?.state !== 'fresh' || !runtime.snapshot) { box.append(element('p', runtime?.state === 'stale' ? `运行观测已过期（最后更新：${date(runtime.snapshot?.observed_at)}）；健康和在线状态未知。` : '无法读取运行观测；请确认 Relay 已升级、正在运行，并与管理服务使用同一服务用户和身份目录。')); return; } const s = runtime.snapshot; box.append(element('p', `采样：${date(s.observed_at)} · Relay 构建：${s.build || '未知'} · 启动：${date(s.started_at)}`)); const list = element('ul'); for (const line of [`信令：${s.running ? 'Relay 正在运行' : 'Relay 已停止'} · 本管理范围 WSS 连接：${s.online_count}`, `身份数据库：${s.identity_healthy ? '查询正常' : '查询失败'}`, activityLabel(s.turn, 'TURN 凭据签发'), activityLabel(s.apns, 'APNs 推送')]) list.append(element('li', line)); box.append(list); if (!s.running) box.append(element('p', '进程已停止，上述配置及请求结果仅为退出时记录。')); }
function renderRequests(items) {
  if (!changed('requests', items)) return;
  const container = $('#requests'); container.replaceChildren();
  if (!items.length) { container.append(element('p', '暂无注册申请。请在 Mac Web 提交或导入申请文件。')); return; }
  for (const item of items) {
    const request = item.request;
    const row = element('div', undefined, 'request');
    row.append(element('strong', `${request.name} · ${states[item.state] || item.state}`));
    row.append(element('p', `申请：${request.id}`, 'mono'));
    row.append(element('p', `服务器：${request.relay_url} · 到期：${date(request.expires_at)}`));
    // Show a fingerprint instead of exposing serialized key bytes in the UI.
    const details = element('p', '公钥指纹计算中…', 'mono'); row.append(details);
    fingerprint(request.public_key).then(value => { details.textContent = '公钥 SHA-256：' + value; }).catch(() => { details.textContent = '指纹计算失败，请暂缓批准'; });
    if (!selections.has(request.id)) selections.set(request.id, new Set(item.approved_modules?.length ? item.approved_modules : request.modules.map(module => module.id)));
    const selected = selections.get(request.id);
    for (const module of request.modules) {
      const label = element('label', undefined, 'module-option');
      const input = element('input'); input.type = 'checkbox'; input.checked = selected.has(module.id); input.disabled = item.state !== 'pending';
      input.addEventListener('change', () => { if (input.checked) selected.add(module.id); else selected.delete(module.id); });
      label.append(input, document.createTextNode(module.alias || '未命名模块'), element('small', module.id, 'mono')); row.append(label);
    }
    const actions = element('div', undefined, 'actions');
    actions.append(copyButton('复制申请编号', request.id), copyButton('复制公钥指纹', () => fingerprint(request.public_key)));
    if (item.state === 'pending') actions.append(button('批准所选模块', async () => {
      const modules = Array.from(selected); if (!modules.length) throw new Error('请至少选择一个申请中的模块');
      await api(`requests/${encodeURIComponent(request.id)}/approve`, { modules }); notice('申请已批准，可下载授权文件。');
    }));
    if (item.state === 'approved') actions.append(button('签发 / 重新签发并下载', async () => {
      if (!await confirmAction('签发新的授权文件？此前未消费的授权文件将失效。')) return;
      const blob = await api(`requests/${encodeURIComponent(request.id)}/authorization`, {}, true);
      const url = URL.createObjectURL(blob); const link = element('a'); link.href = url; link.download = 'djonehub-enrollment.json';
      document.body.append(link); link.click(); link.remove(); setTimeout(() => URL.revokeObjectURL(url), 1000);
      notice('授权文件已下载。请在 Mac Web 导入；完成后删除交付文件。');
    }));
    if (['pending', 'approved'].includes(item.state)) actions.append(button('拒绝 / 撤销未完成授权', async () => {
      if (!await confirmAction('拒绝此申请并撤销尚未消费的授权？')) return;
      await api(`requests/${encodeURIComponent(request.id)}/reject`, {}); notice('申请已拒绝，旧授权已失效。');
    }, true));
    if (item.authorization) {
      const grant = item.authorization;
      const labels = { pending: '已签发，等待导入', consumed: '已消费', revoked: '已撤销', expired: '已过期' };
      row.append(element('p', `最新授权：${labels[grant.state] || grant.state} · 有效期至 ${date(grant.expires_at)}`));
      actions.append(button('删除此授权', async () => {
        if (!await confirmAction(`删除授权 ${grant.id}？未消费的下载副本将立即失效；已注册 Mac 与手机不会被撤销。这里只清理服务器记录，浏览器下载文件需自行删除。`)) return;
        await api(`requests/${encodeURIComponent(request.id)}/authorizations/${encodeURIComponent(grant.id)}/delete`, {});
        notice('授权记录已删除；旧文件不能再用于登记，已注册设备不受影响。');
      }, true));
    }
    actions.append(button('删除申请及授权', async () => {
      if (!await confirmAction(`删除申请 ${request.name}（${request.id}）及其所有授权记录？尚未消费的下载文件将失效；已注册 Mac 与 iPhone 不受影响。操作不能恢复，管理审计保留。`)) return;
      const result = await api(`requests/${encodeURIComponent(request.id)}/delete`, {});
      selections.delete(request.id);
      notice(`注册申请及 ${result.deleted_authorizations} 条授权记录已删除；已注册设备不受影响。`);
    }, true));
    if (item.hub_id) row.append(element('p', '已登记 Hub：' + item.hub_id, 'mono'));
    row.append(actions); container.append(row);
  }
}
async function fingerprint(value) {
  const bytes = Uint8Array.from(atob(value), char => char.charCodeAt(0));
  const digest = await crypto.subtle.digest('SHA-256', bytes);
  return Array.from(new Uint8Array(digest), byte => byte.toString(16).padStart(2, '0')).join('');
}
function renderIdentities(items) {
  if (!changed('identities', items)) return;
  const container = $('#identities'); container.replaceChildren();
  if (!items.length) { const row = element('tr'); const cell = element('td', '暂无已注册设备'); cell.colSpan = 5; row.append(cell); container.append(row); return; }
  for (const item of items) {
    const row = element('tr'); row.id = 'identity-' + item.id; const name = element('td');
    name.append(element('strong', `${item.name || '未命名'} · ${item.kind === 'hub' ? 'Mac' : 'iPhone'}`), element('p', item.id, 'mono'), element('p', '公钥：' + item.public_key_fingerprint, 'mono'));
    const owner = element('td'); const ownerLink = element('a', item.hub_id, 'mono'); ownerLink.href = '#identity-' + encodeURIComponent(item.hub_id); owner.append(ownerLink);
    const scope = element('td', item.modules.join(', '));
    const state = element('td', item.revoked ? '已撤销' : '有效'); state.append(element('p', signalLabel(item), 'connection-state'), element('p', '连接采样：' + date(item.signal_observed_at), 'connection-time'));
    state.append(element('p', '注册：' + date(item.created_at)), element('p', '最近认证：' + date(item.last_authenticated)));
    if (item.kind === 'installation') state.append(element('p', item.push_registered ? `已登记 ${item.push_environment} 推送；不代表当前在线` : '未登记推送'));
    const operations = element('td'); operations.append(copyButton('复制身份 ID', item.id), copyButton('复制公钥指纹', item.public_key_fingerprint));
    if (!item.revoked) operations.append(button('撤销', async () => {
      if (!await confirmAction(`撤销 ${item.name || item.id}？${item.kind === 'hub' ? '所属 iPhone 将一起失去授权。' : ''}活动通话可能中断。`)) return;
      await api(`identities/${encodeURIComponent(item.id)}/revoke`, {}); notice('身份已撤销。请在 Mac 更新相关模块的接听目标。');
    }, true));
    if (item.revoked) operations.append(button('删除记录', async () => {
      if (!await confirmAction(`永久删除已撤销的 ${item.name || item.id}？${item.kind === 'hub' ? '该 Mac 及其所属已失效 iPhone 的设备记录将一并删除。' : ''}关联凭据将清理，审批历史和审计记录仍保留。此操作不能恢复；不会清理 Mac 或手机本机的配置。`)) return;
      const result = await api(`identities/${encodeURIComponent(item.id)}/delete`, {});
      notice(`已删除 ${result.deleted_count} 条已撤销设备记录。审批历史和审计记录已保留。`);
    }, true));
    row.append(name, owner, scope, state, operations); container.append(row);
  }
}
function renderAudit(items) {
  if (!changed('audit', items)) return;
  const container = $('#audit'); container.replaceChildren();
  for (const item of items) container.append(element('div', `${date(item.at)} · ${item.username} · ${actionNames[item.action] || item.action} · ${item.object_id} · ${resultNames[item.result] || item.result}`, 'audit-entry'));
  if (!items.length) container.append(element('p', '暂无管理操作'));
}
async function clearAudit() {
  if (!await confirmAction('清除当前管理范围内所有已完成（成功或失败）的操作记录？不只清除当前显示的 256 条。待核对 / 正在执行的记录保留，并记录本次清除。此操作不能恢复，不改变申请、授权或设备。')) return;
  const result = await api('audit/clear', {});
  notice(`已清除 ${result.cleared_count} 条已完成管理记录；待核对记录和本次清除记录保留。`);
}
async function refresh() {
  if (!authenticated || loading) return;
  loading = true;
  try {
    const [overview, requests, identities, audit] = await Promise.all([api('overview'), api('requests'), api('identities'), api('audit')]);
    if (!authenticated) return;
    $('#overview').textContent = `Relay：${overview.relay_origin} · 管理范围：${overview.tenant} · 构建：${overview.build}`;
    $('#request-count').textContent = requests.requests.filter(item => item.state === 'pending').length;
    $('#mac-count').textContent = identities.identities.filter(item => item.kind === 'hub' && !item.revoked).length;
    $('#phone-count').textContent = identities.identities.filter(item => item.kind === 'installation' && !item.revoked).length;
    $('#last-updated').textContent = `更新于 ${new Date().toLocaleTimeString()} · 每五秒自动刷新`;
    runtimeReceivedAt = Date.now(); lastRuntime = overview.runtime; identities.identities.forEach(item => { item.signal_received_at = runtimeReceivedAt; }); renderHealth(overview.runtime); renderRequests(requests.requests); renderIdentities(identities.identities); renderAudit(audit.events);
  } catch (error) { renderHealth({ state: 'unavailable' }); $('#identities').querySelectorAll('.connection-state').forEach(item => { item.textContent = 'WSS 在线状态未知'; }); $('#identities').querySelectorAll('.connection-time').forEach(item => { item.textContent = '本次查询失败'; }); notice(error.message); } finally { loading = false; }
}
$('#login-form').addEventListener('submit', async event => {
  event.preventDefault(); const node = $('#login-button'); if (busy) return;
  busy = true; node.disabled = true;
  const body = { username: $('#username').value, password: $('#password').value };
  if (!configured) body.token = $('#setup-token').value;
  try {
    const session = await api(configured ? 'session/login' : 'session/setup', body);
    csrf = session.csrf; authenticated = true; configured = true;
    $('#password').value = ''; $('#setup-token').value = '';
    $('#login-panel').hidden = true; $('#management').hidden = false; $('#logout').hidden = false;
    notice('已登录'); await refresh();
  } catch (error) { notice(error.message); } finally { busy = false; node.disabled = false; }
});
$('#logout').addEventListener('click', () => action($('#logout'), async () => { await api('session/logout', {}); showLogin(); notice('已退出'); }));
$('#refresh').addEventListener('click', () => action($('#refresh'), async () => {}));
$('#clear-audit').addEventListener('click', () => action($('#clear-audit'), clearAudit));
$('#import-request').addEventListener('change', event => {
  const input = event.currentTarget;
  const file = input.files[0];
  if (!file) return;
  action(input, async () => {
    if (file.size > 16384) throw new Error('申请文件超过 16 KiB');
    const request = JSON.parse(await file.text());
    await api('requests/import', request); notice('申请已导入，请核对模块和公钥指纹。');
  }).finally(() => { input.value = ''; });
});
(async () => {
  try {
    const session = await api('session/status'); configured = session.configured;
    if (session.authenticated) { authenticated = true; csrf = session.csrf; $('#login-panel').hidden = true; $('#management').hidden = false; $('#logout').hidden = false; await refresh(); }
    else showLogin();
  } catch (error) { showLogin(); notice(error.message); }
})();
setInterval(() => { if (!document.hidden && !busy) void refresh(); }, 5000);

setInterval(() => { if (authenticated && runtimeReceivedAt && Date.now() - runtimeReceivedAt > 12000) { renderHealth({state: 'stale', snapshot: {observed_at: lastRuntime?.snapshot?.observed_at}}); $('#identities').querySelectorAll('.connection-state').forEach(item => { item.textContent = 'WSS 在线状态未知'; }); } }, 1000);
