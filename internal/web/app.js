'use strict';
const $ = id => document.getElementById(id);
const adminPage = document.body.dataset.page === 'admin';
function on(id, event, action) { $(id)?.addEventListener(event, action); }
let config = {}, me = null, nodes = [], refreshBusy = false;
const expandedNodes = new Map();
function show(id, visible = true) { const e = $(id); if (e) e.hidden = !visible; }
function el(tag, text, cls) { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (cls) e.className = cls; return e; }
function notice(message, error = false) { const target = $('ssh-dialog')?.open ? $('ssh-feedback') : $('notice'); target.textContent = message; target.className = error ? 'error' : ''; target.hidden = false; }
function button(text, action, cls = 'secondary') { const b = el('button', text, cls); b.type = 'button'; b.addEventListener('click', async () => { b.disabled = true; try { await action(); } catch (e) { notice(e.message, true); } finally { b.disabled = false; } }); return b; }
async function api(path, method = 'GET', body) {
  const r = await fetch(path, { method, credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-NodeBridge-Request': '1' }, body: body === undefined ? undefined : JSON.stringify(body) });
  let data; try { data = await r.json(); } catch { throw Error('服务响应异常，请检查连接'); }
  if (!r.ok) { const e = Error(data.error || '请求失败'); e.status = r.status; throw e; } return data;
}
function form(id, fn) {
  if (!$(id)) return;
  $(id).addEventListener('submit', async e => {
    e.preventDefault(); const controls = [...e.target.querySelectorAll('button')]; controls.forEach(b => b.disabled = true);
    try { await fn(new FormData(e.target), e.target); } catch (err) { notice(err.message, true); } finally { controls.forEach(b => b.disabled = false); }
  });
}
async function copy(value) {
  try { await navigator.clipboard.writeText(value); notice('已复制'); } catch { notice('复制失败，请手动复制', true); }
}
function date(value) { return value ? new Date(value).toLocaleString() : '—'; }
function metric(value, suffix = '') { return value == null ? '—' : `${Number(value).toFixed(0)}${suffix}`; }

async function boot() {
  config = await api('/api/config'); show('loading', false);
  if (config.mode === 'node') { if (adminPage) { location.replace('/'); return; } show('node-page'); await refreshNode(); return; }
  if ($('allow-register')) $('allow-register').checked = config.allow_register;
  show('register-button', config.allow_register);
  try { me = await api('/api/me'); await dashboard(); } catch (e) { if (e.status === 401) { show('auth'); } else { throw e; } }
}
async function dashboard() {
  if (adminPage && !me.admin) { location.replace('/'); return; }
  show('auth', false); show('dashboard', !adminPage); show('admin-panel', adminPage);
  const toolbar = $('toolbar'); toolbar.replaceChildren(el('span', `${me.username}${me.admin ? ' · 管理员' : ''}`));
  if (me.admin) {
    const link = el('a', adminPage ? '服务器一览' : '管理页面', 'toolbar-link'); link.href = adminPage ? '/' : '/admin'; toolbar.append(link);
  }
  toolbar.append(button('修改密码', changePassword), button('退出', logout));
  if (adminPage) { activateTab(location.hash.slice(1)); await Promise.all([refreshAdminNodes(), refreshUsers(), refreshAudit()]); } else await refreshNodes();
}
async function logout() { await api('/api/logout', 'POST', {}); me = null; location.reload(); }
function nodeState(n) {
  const paused = n.hub_paused || n.forwarding_paused || n.node_paused;
  return { text: paused ? 'SSH 转发已暂停' : n.ssh_ready ? 'SSH 就绪' : n.online ? '在线 · SSH 未就绪' : '离线', cls: paused ? '' : n.ssh_ready ? 'good' : n.online ? 'bad' : '' };
}
function nodePauseButton(n, refresh) {
  return button(n.forwarding_paused ? '恢复节点转发' : '暂停节点转发', async () => {
    await toggleForwarding(`/api/nodes/${encodeURIComponent(n.id)}/forwarding`, !n.forwarding_paused, n.name);
    await refresh();
  });
}
function emptyNodes(content, admin = false) {
  const empty = el('div', undefined, 'empty-state');
  empty.append(el('h3', '还没有接入服务器'), el('p', admin ? '点击“接入服务器”，生成第一台服务器的配对链接。' : me.admin ? '在管理页面接入服务器后，即可在这里连接。' : '管理员接入服务器后，会显示在这里。', 'muted'));
  content.append(empty);
}
function openSSH(id) {
  $('ssh-form').elements.node.value = id;
  show('ssh-result', false); show('ssh-feedback', false);
  $('ssh-dialog').showModal();
  $('ssh-form').elements.username.focus();
}
function listFocus(content) {
  const active = document.activeElement, row = active.closest('[data-node-id]');
  if (!row || !content.contains(active)) return null;
  return { id: row.dataset.nodeId, index: [...row.querySelectorAll('button, summary')].indexOf(active) };
}
function restoreListFocus(content, focus) {
  if (!focus || document.activeElement !== document.body) return;
  const row = [...content.querySelectorAll('[data-node-id]')].find(row => row.dataset.nodeId === focus.id);
  row?.querySelectorAll('button, summary')[focus.index]?.focus({ preventScroll: true });
}
async function refreshNodes() {
  nodes = await api('/api/nodes');
  $('node-summary').textContent = `${nodes.filter(n => n.online).length} / ${nodes.length} 在线`;
  const content = $('nodes'), focus = listFocus(content); content.replaceChildren();
  if (!nodes.length) emptyNodes(content);
  const select = $('ssh-form').elements.node, selected = select.value;
  const optionsKey = JSON.stringify(nodes.map(n => [n.id, n.name, n.ssh_ready]));
  if (select.dataset.nodes !== optionsKey) {
    select.replaceChildren();
    for (const n of nodes) {
      const option = el('option', `${n.name}${n.ssh_ready ? '' : '（未就绪）'}`); option.value = n.id; select.append(option);
    }
    if (nodes.some(n => n.id === selected)) select.value = selected;
    else show('ssh-result', false);
    if (!nodes.find(n => n.id === select.value)?.ssh_ready) show('ssh-result', false);
    select.dataset.nodes = optionsKey;
  }
  for (const n of nodes) {
    const row = el('article', undefined, 'node'), head = el('div', undefined, 'node-head'), info = el('div', undefined, 'node-info');
    row.dataset.nodeId = n.id;
    const title = el('div', undefined, 'node-title'), status = nodeState(n);
    title.append(el('h3', n.name), el('span', status.text, `badge ${status.cls}`));
    info.append(title, el('p', `${n.host}:${n.port}`, 'node-address'));
    const actions = el('div', undefined, 'actions');
    const connect = button('SSH 连接', () => openSSH(n.id), 'primary'); connect.disabled = !n.ssh_ready;
    actions.append(connect); if (me.admin) actions.append(nodePauseButton(n, refreshNodes));
    head.append(info, actions); row.append(head);
    if (n.hub_paused || n.forwarding_paused || n.node_paused) row.append(el('p', `暂停来源：${[n.hub_paused && 'VPS 全局', n.forwarding_paused && 'VPS 节点设置', n.node_paused && '节点本机'].filter(Boolean).join('、')}`, 'muted'));
    if (n.error) row.append(el('p', n.error, 'muted'));
    if (!n.online && n.last_seen && !n.last_seen.startsWith('0001')) row.append(el('p', `最后在线 ${date(n.last_seen)}`, 'muted'));
    const detail = el('details', undefined, 'node-details');
    detail.append(el('summary', '运行详情')); detail.open = expandedNodes.get(n.id) || false;
    detail.addEventListener('toggle', () => { if (detail.isConnected) expandedNodes.set(n.id, detail.open); });
    if (n.status?.hostname) detail.append(el('p', `${n.status.hostname} · ${n.status.cpus} 核 · ${n.status.arch}`, 'muted'));
    if (n.online && n.status) {
      for (const g of n.status.gpus || []) {
        const gpu = el('div', undefined, 'gpu'); gpu.append(el('strong', `${g.index} · ${g.name}`));
        gpu.append(el('span', `利用率 ${metric(g.utilization, '%')} · 显存 ${metric(g.memory_used_mb)} / ${metric(g.memory_total_mb)} MB`));
        const meter = el('progress', undefined, 'gpu-meter'); meter.max = 100; meter.value = g.utilization || 0; meter.setAttribute('aria-label', 'GPU 利用率'); gpu.append(meter);
        gpu.append(el('span', `温度 ${metric(g.temperature, '°C')} · 功耗 ${metric(g.power_watts, ' W')}`)); detail.append(gpu);
      }
      if (n.status.gpu_error) detail.append(el('p', n.status.gpu_error, 'muted'));
    }
    if (n.history) {
      detail.append(el('h4', '过去 30 天在线记录'));
      const strip = el('div', undefined, 'history-strip');
      for (const day of n.history) {
        const percentage = day.checks ? Math.round(day.online / day.checks * 100) : null;
        const cell = el('span', undefined, percentage === null ? 'unknown' : percentage >= 95 ? 'good' : percentage > 0 ? 'partial' : 'bad');
        cell.title = `${day.date} · ${percentage === null ? '未采样' : `在线 ${percentage}% · SSH 就绪 ${Math.round(day.ssh_ready / day.checks * 100)}% · ${day.checks} 次采样`}`; strip.append(cell);
      }
      detail.append(strip);
    }
    row.append(detail); content.append(row);
  }
  restoreListFocus(content, focus);
  for (const id of expandedNodes.keys()) if (!nodes.some(n => n.id === id)) expandedNodes.delete(id);
}
async function refreshAdminNodes() {
  const [savedNodes, forwarding] = await Promise.all([api('/api/nodes'), api('/api/forwarding')]);
  forwardingControl('hub-forwarding', forwarding.paused, '/api/forwarding', '全部节点', refreshAdminNodes);
  $('admin-node-summary').textContent = `${savedNodes.length} 台服务器 · ${savedNodes.filter(n => n.online).length} 台在线`;
  const content = $('admin-nodes'), focus = listFocus(content); content.replaceChildren();
  if (!savedNodes.length) emptyNodes(content, true);
  for (const n of savedNodes) {
    const row = el('div', undefined, 'admin-node-row'), info = el('div', undefined, 'node-info'), title = el('div', undefined, 'node-title'), status = nodeState(n);
    row.dataset.nodeId = n.id;
    title.append(el('h3', n.name), el('span', status.text, `badge ${status.cls}`));
    info.append(title, el('p', `${n.host}:${n.port}`, 'node-address'));
    const actions = el('div', undefined, 'actions');
    actions.append(nodePauseButton(n, refreshAdminNodes), button('移除节点', async () => {
      if (prompt(`输入「${n.name}」确认移除。配对凭证将撤销，SSH 会话将断开。`) !== n.name) return;
      await api(`/api/nodes/${encodeURIComponent(n.id)}`, 'DELETE', {}); notice('节点已移除'); await refreshAdminNodes();
    }, 'secondary danger destructive-action'));
    row.append(info, actions); content.append(row);
  }
  restoreListFocus(content, focus);
}
async function refreshNode() {
  const s = await api('/api/node'); const content = $('local-status'); content.replaceChildren();
  content.append(el('span', s.online ? '已连接' : s.paired ? '正在连接控制台' : '等待配对', `badge ${s.online ? 'good' : ''}`));
  if (s.hub_url) content.append(el('p', s.hub_url, 'muted'));
  content.append(el('p', `本地 SSH 端口：${s.ssh_port}`, 'muted'));
  if (s.error) content.append(el('p', s.error, 'muted'));
  if (s.port) content.append(el('p', `固定公网端口：${s.port}`, 'muted'));
  forwardingControl('local-forwarding', s.forwarding_paused, '/api/forwarding', '这台节点', refreshNode);
  show('pair-section', !s.paired);
}
async function toggleForwarding(path, paused, scope) {
  if (paused && !confirm(`暂停${scope}的 SSH 转发？现有 SSH 会话和文件传输将断开。`)) return;
  await api(path, 'PUT', { paused });
  notice(paused ? 'SSH 转发已暂停' : '已恢复此处的转发设置');
}
function forwardingControl(id, paused, path, scope, refresh) {
  $(id).replaceChildren(el('span', paused ? 'SSH 转发已暂停' : 'SSH 转发已启用', `badge ${paused ? '' : 'good'}`), button(paused ? '恢复 SSH 转发' : '暂停 SSH 转发', async () => { await toggleForwarding(path, !paused, scope); await refresh(); }));
}
async function refreshUsers() {
  const users = await api('/api/users'); $('users').replaceChildren();
  for (const user of users) {
    const row = el('tr'); row.append(el('td', user.username), el('td', user.admin ? '管理员' : '普通用户'));
    const cell = el('td'), actions = el('div', undefined, 'actions');
    if (user.username !== me.username) {
      actions.append(button(user.admin ? '设为普通用户' : '设为管理员', async () => { await api(`/api/users/${encodeURIComponent(user.username)}`, 'PATCH', { admin: !user.admin }); await refreshUsers(); }));
      actions.append(button('重置密码', async () => { const result = await passwordDialog(`重置 ${user.username} 的密码`, false); if (!result) return; await api(`/api/users/${encodeURIComponent(user.username)}`, 'PATCH', { password: result.password }); notice('密码已重置，原有登录已失效'); }));
      actions.append(button('删除', async () => { if (!confirm(`删除控制台账号 ${user.username}？`)) return; await api(`/api/users/${encodeURIComponent(user.username)}`, 'DELETE', {}); await refreshUsers(); }, 'secondary danger'));
    } else actions.append(el('span', '当前账号', 'muted'));
    cell.append(actions); row.append(cell); $('users').append(row);
  }
}
async function refreshAudit() {
  const audit = await api('/api/audit'); $('audit').replaceChildren();
  for (const entry of audit.slice(-50).reverse()) $('audit').append(el('p', `${date(entry.at)} · ${entry.actor} · ${entry.action}${entry.target ? ' · ' + entry.target : ''}`));
}
function passwordDialog(title, current) {
  return new Promise(resolve => {
    const dialog = el('dialog', undefined, 'card'), f = el('form'); f.className = 'row'; dialog.append(el('h2', title), f);
    const fields = {};
    for (const [name, label] of [...(current ? [['current', '当前密码']] : []), ['password', '新密码（至少 10 字节）']]) {
      const l = el('label', label), input = el('input'); input.type = 'password'; input.required = true; input.maxLength = 72; input.autocomplete = name === 'current' ? 'current-password' : 'new-password'; if (name === 'password') input.minLength = 10; fields[name] = input; l.append(input); f.append(l);
    }
    const submit = el('button', '保存'); submit.type = 'submit'; f.append(submit, button('取消', () => dialog.close()));
    let value = null; f.addEventListener('submit', e => { e.preventDefault(); value = Object.fromEntries(Object.entries(fields).map(([k, v]) => [k, v.value])); dialog.close(); });
    dialog.addEventListener('close', () => { dialog.remove(); resolve(value); }, { once: true }); document.body.append(dialog); dialog.showModal();
  });
}
async function changePassword() {
  const p = await passwordDialog('修改登录密码', true); if (!p) return; await api('/api/password', 'PUT', p); me = null; location.reload();
}
form('login-form', async data => { me = await api('/api/login', 'POST', { username: data.get('username'), password: data.get('password') }); $('login-form').elements.password.value = ''; show('notice', false); if (adminPage) { location.assign('/'); return; } await dashboard(); });
on('register-button', 'click', async () => {
  const f = $('login-form'); if (!f.reportValidity()) return;
  try { await api('/api/register', 'POST', { username: f.elements.username.value, password: f.elements.password.value }); notice('账号已创建，请登录'); f.elements.password.value = ''; } catch (e) { notice(e.message, true); }
});
form('pair-form', async (data, f) => { await api('/api/pair', 'POST', { link: data.get('link').trim() }); f.reset(); notice('配对成功'); await refreshNode(); });
form('invite-form', async data => { const result = await api('/api/invites', 'POST', { name: data.get('name') }); $('pair-link').value = result.link; $('invite-expiry').textContent = `截止 ${date(result.expires)} · 单次使用`; show('invite-result'); });
form('ssh-form', async data => {
  const n = nodes.find(n => n.id === data.get('node')); if (!n || !n.ssh_ready) throw Error('节点 SSH 尚未就绪，请等待连接或检查节点 SSH 服务');
  const user = data.get('username'); if (!/^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,63}$/.test(user)) throw Error('Linux 用户名无效');
  $('ssh-command').textContent = `ssh -p ${n.port} ${user}@${n.host}`; show('ssh-result');
});
form('user-form', async (data, f) => { await api('/api/users', 'POST', { username: data.get('username'), password: data.get('password'), admin: data.has('admin') }); f.reset(); notice('账号已创建'); await refreshUsers(); });
on('copy-link', 'click', () => copy($('pair-link').value));
on('copy-ssh', 'click', () => copy($('ssh-command').textContent));
on('audit-refresh', 'click', () => refreshAudit().catch(e => notice(e.message, true)));
on('allow-register', 'change', async e => {
  try { await api('/api/settings', 'PUT', { allow_register: e.target.checked }); notice('注册设置已保存'); } catch (err) { e.target.checked = !e.target.checked; notice(err.message, true); }
});
function activateTab(name, updateURL = false) {
  const tabs = [...document.querySelectorAll('[role="tab"][data-tab]')];
  if (!tabs.length) return;
  if (!tabs.some(tab => tab.dataset.tab === name)) name = 'servers';
  for (const tab of tabs) {
    const selected = tab.dataset.tab === name;
    tab.setAttribute('aria-selected', String(selected)); tab.tabIndex = selected ? 0 : -1;
    show(`panel-${tab.dataset.tab}`, selected);
  }
  if (updateURL) history.replaceState(null, '', `#${name}`);
}
for (const tab of document.querySelectorAll('[role="tab"][data-tab]')) {
  tab.addEventListener('click', () => activateTab(tab.dataset.tab, true));
  tab.addEventListener('keydown', event => {
    const tabs = [...document.querySelectorAll('[role="tab"][data-tab]')], index = tabs.indexOf(tab);
    let next;
    if (event.key === 'ArrowRight') next = tabs[(index + 1) % tabs.length];
    if (event.key === 'ArrowLeft') next = tabs[(index - 1 + tabs.length) % tabs.length];
    if (event.key === 'Home') next = tabs[0];
    if (event.key === 'End') next = tabs[tabs.length - 1];
    if (next) { event.preventDefault(); activateTab(next.dataset.tab, true); next.focus(); }
  });
}
window.addEventListener('hashchange', () => { if (adminPage) activateTab(location.hash.slice(1)); });
function toggleInline(buttonId, sectionId, inputName) {
  const section = $(sectionId), control = $(buttonId); section.hidden = !section.hidden;
  control.setAttribute('aria-expanded', String(!section.hidden));
  if (!section.hidden) section.querySelector(`[name="${inputName}"]`).focus();
}
on('invite-toggle', 'click', () => toggleInline('invite-toggle', 'invite-section', 'name'));
on('user-toggle', 'click', () => toggleInline('user-toggle', 'user-section', 'username'));
on('ssh-close', 'click', () => $('ssh-dialog').close());
on('ssh-dialog', 'close', () => show('ssh-feedback', false));
on('ssh-form', 'input', () => { show('ssh-result', false); show('ssh-feedback', false); });
setInterval(async () => {
  if (refreshBusy || document.hidden) return; refreshBusy = true;
  try { if (config.mode === 'node') await refreshNode(); else if (me) { if (adminPage) await refreshAdminNodes(); else await refreshNodes(); } } catch (e) { if (e.status === 401) { me = null; $('ssh-dialog')?.close(); show('dashboard', false); show('admin-panel', false); show('auth'); $('toolbar').replaceChildren(); } else notice(e.message, true); } finally { refreshBusy = false; }
}, 5000);
boot().catch(e => { show('loading', false); notice(e.message, true); });
