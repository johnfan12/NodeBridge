'use strict';
const $ = id => document.getElementById(id);
let config = {}, me = null, nodes = [], refreshBusy = false;
function show(id, visible = true) { $(id).hidden = !visible; }
function el(tag, text, cls) { const e = document.createElement(tag); if (text !== undefined) e.textContent = text; if (cls) e.className = cls; return e; }
function notice(message, error = false) { $('notice').textContent = message; $('notice').className = error ? 'error' : ''; show('notice'); }
function button(text, action, cls = 'secondary') { const b = el('button', text, cls); b.type = 'button'; b.addEventListener('click', () => Promise.resolve(action()).catch(e => notice(e.message, true))); return b; }
async function api(path, method = 'GET', body) {
  const r = await fetch(path, { method, credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-NodeBridge-Request': '1' }, body: body === undefined ? undefined : JSON.stringify(body) });
  let data; try { data = await r.json(); } catch { throw Error('服务响应异常，请检查连接'); }
  if (!r.ok) { const e = Error(data.error || '请求失败'); e.status = r.status; throw e; } return data;
}
function form(id, fn) {
  $(id).addEventListener('submit', async e => {
    e.preventDefault(); const controls = [...e.target.querySelectorAll('button')]; controls.forEach(b => b.disabled = true);
    try { await fn(new FormData(e.target), e.target); } catch (err) { notice(err.message, true); } finally { controls.forEach(b => b.disabled = false); }
  });
}
async function copy(value) {
  try { await navigator.clipboard.writeText(value); notice('已复制'); } catch { notice('浏览器未允许自动复制，请选中文字手动复制', true); }
}
function date(value) { return value ? new Date(value).toLocaleString() : '—'; }
function metric(value, suffix = '') { return value == null ? '—' : `${Number(value).toFixed(0)}${suffix}`; }

async function boot() {
  config = await api('/api/config'); show('loading', false);
  if (config.mode === 'node') { show('node-page'); await refreshNode(); return; }
  $('allow-register').checked = config.allow_register;
  show('register-button', config.allow_register);
  try { me = await api('/api/me'); await dashboard(); } catch (e) { if (e.status === 401) { show('auth'); } else { throw e; } }
}
async function dashboard() {
  show('auth', false); show('dashboard'); show('admin-panel', me.admin);
  $('toolbar').replaceChildren(el('span', `${me.username}${me.admin ? ' · 管理员' : ''}`), button('修改密码', changePassword), button('退出', logout));
  await refreshNodes(); if (me.admin) { await Promise.all([refreshUsers(), refreshAudit()]); }
}
async function logout() { await api('/api/logout', 'POST', {}); me = null; location.reload(); }
async function refreshNodes() {
  nodes = await api('/api/nodes');
  $('node-summary').textContent = `${nodes.filter(n => n.online).length} / ${nodes.length} 在线`;
  const content = $('nodes'); content.replaceChildren();
  if (!nodes.length) content.append(el('p', me.admin ? '还没有服务器。生成配对链接，接入第一台节点。' : '管理员尚未接入服务器。', 'muted'));
  const selected = $('ssh-form').elements.node.value;
  const select = $('ssh-form').elements.node; select.replaceChildren();
  for (const n of nodes) {
    const option = el('option', `${n.name}${n.ssh_ready ? '' : '（未就绪）'}`); option.value = n.id; select.append(option);
    const card = el('article', undefined, 'node'), top = el('div', undefined, 'node-top');
    const status = n.ssh_ready ? 'SSH 就绪' : n.online ? '在线 · SSH 未就绪' : '离线';
    top.append(el('h3', n.name), el('span', status, `badge ${n.ssh_ready ? 'good' : n.online ? 'bad' : ''}`)); card.append(top);
    card.append(el('p', `${n.host}:${n.port}`, 'muted'));
    if (n.status && n.status.hostname) card.append(el('p', `${n.status.hostname} · ${n.status.cpus} 核 · ${n.status.arch}`, 'muted'));
    if (n.error) card.append(el('p', n.error, 'muted'));
    if (!n.online && n.last_seen && !n.last_seen.startsWith('0001')) card.append(el('p', `最后在线 ${date(n.last_seen)}`, 'muted'));
    if (n.history) {
      const detail = el('details', undefined, 'history-detail'); detail.append(el('summary', '过去 30 天在线记录'));
      const strip = el('div', undefined, 'history-strip');
      for (const day of n.history) {
        const percentage = day.checks ? Math.round(day.online / day.checks * 100) : null;
        const cell = el('span', undefined, percentage === null ? 'unknown' : percentage >= 95 ? 'good' : percentage > 0 ? 'partial' : 'bad');
        cell.title = `${day.date} · ${percentage === null ? '未采样' : `在线 ${percentage}% · SSH 就绪 ${Math.round(day.ssh_ready / day.checks * 100)}% · ${day.checks} 次采样`}`; strip.append(cell);
      }
      detail.append(strip, el('p', '每 30 秒采样，灰色表示无数据。', 'muted')); card.append(detail);
    }
    if (n.online && n.status) {
      for (const g of n.status.gpus || []) {
        const gpu = el('div', undefined, 'gpu'); gpu.append(el('strong', `${g.index} · ${g.name}`));
        gpu.append(el('span', `利用率 ${metric(g.utilization, '%')} · 显存 ${metric(g.memory_used_mb)} / ${metric(g.memory_total_mb)} MB`));
        const meter = el('progress', undefined, 'gpu-meter'); meter.max = 100; meter.value = g.utilization || 0; meter.setAttribute('aria-label', 'GPU 利用率'); gpu.append(meter);
        gpu.append(el('span', `温度 ${metric(g.temperature, '°C')} · 功耗 ${metric(g.power_watts, ' W')}`)); card.append(gpu);
      }
      if (n.status.gpu_error) card.append(el('p', n.status.gpu_error, 'muted'));
    }
    if (me.admin) card.append(button('移除节点', async () => {
      if (prompt(`输入节点名称「${n.name}」确认移除。现有 SSH 隧道将断开。`) !== n.name) return;
      await api(`/api/nodes/${encodeURIComponent(n.id)}`, 'DELETE', {}); notice('节点已移除，连接凭证已撤销'); await refreshNodes();
    }, 'secondary danger'));
    content.append(card);
  }
  if (nodes.some(n => n.id === selected)) select.value = selected;
}
async function refreshNode() {
  const s = await api('/api/node'); const content = $('local-status'); content.replaceChildren();
  content.append(el('span', s.online ? '已连接' : s.paired ? '正在连接控制台' : '等待配对', `badge ${s.online ? 'good' : ''}`));
  if (s.hub_url) content.append(el('p', s.hub_url, 'muted'));
  content.append(el('p', `本地 SSH 端口：${s.ssh_port}`, 'muted'));
  if (s.error) content.append(el('p', s.error, 'muted'));
  show('pair-section', !s.paired);
}
async function refreshUsers() {
  const users = await api('/api/users'); $('users').replaceChildren();
  for (const user of users) {
    const row = el('tr'); row.append(el('td', user.username), el('td', user.admin ? '管理员' : '普通用户'));
    const actions = el('td');
    if (user.username !== me.username) {
      actions.append(button(user.admin ? '设为普通用户' : '设为管理员', async () => { await api(`/api/users/${encodeURIComponent(user.username)}`, 'PATCH', { admin: !user.admin }); await refreshUsers(); }));
      actions.append(button('重置密码', async () => { const result = await passwordDialog(`重置 ${user.username} 的密码`, false); if (!result) return; await api(`/api/users/${encodeURIComponent(user.username)}`, 'PATCH', { password: result.password }); notice('密码已重置，原有登录已失效'); }));
      actions.append(button('删除', async () => { if (!confirm(`删除控制台账号 ${user.username}？节点 Linux 账号需在节点管理。`)) return; await api(`/api/users/${encodeURIComponent(user.username)}`, 'DELETE', {}); await refreshUsers(); }, 'secondary danger'));
    } else actions.append(el('span', '当前账号', 'muted'));
    row.append(actions); $('users').append(row);
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
form('login-form', async data => { me = await api('/api/login', 'POST', { username: data.get('username'), password: data.get('password') }); $('login-form').elements.password.value = ''; show('notice', false); await dashboard(); });
$('register-button').addEventListener('click', async () => {
  const f = $('login-form'); if (!f.reportValidity()) return;
  try { await api('/api/register', 'POST', { username: f.elements.username.value, password: f.elements.password.value }); notice('账号已创建，请登录'); f.elements.password.value = ''; } catch (e) { notice(e.message, true); }
});
form('pair-form', async (data, f) => { await api('/api/pair', 'POST', { link: data.get('link').trim() }); f.reset(); notice('配对成功，正在建立隧道'); await refreshNode(); });
form('invite-form', async data => { const result = await api('/api/invites', 'POST', { name: data.get('name') }); $('pair-link').value = result.link; $('invite-expiry').textContent = `有效期至 ${date(result.expires)} · 仅可使用一次`; show('invite-result'); });
form('ssh-form', async data => {
  const n = nodes.find(n => n.id === data.get('node')); if (!n || !n.ssh_ready) throw Error('节点 SSH 尚未就绪，请等待连接或检查节点 SSH 服务');
  const user = data.get('username'); if (!/^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,63}$/.test(user)) throw Error('Linux 用户名无效');
  $('ssh-command').textContent = `ssh -p ${n.port} ${user}@${n.host}`; show('ssh-result');
});
form('user-form', async (data, f) => { await api('/api/users', 'POST', { username: data.get('username'), password: data.get('password'), admin: data.has('admin') }); f.reset(); notice('账号已创建'); await refreshUsers(); });
$('copy-link').addEventListener('click', () => copy($('pair-link').value));
$('copy-ssh').addEventListener('click', () => copy($('ssh-command').textContent));
$('audit-refresh').addEventListener('click', () => refreshAudit().catch(e => notice(e.message, true)));
$('allow-register').addEventListener('change', async e => {
  try { await api('/api/settings', 'PUT', { allow_register: e.target.checked }); notice('注册设置已保存'); } catch (err) { e.target.checked = !e.target.checked; notice(err.message, true); }
});
setInterval(async () => {
  if (refreshBusy || document.hidden) return; refreshBusy = true;
  try { if (config.mode === 'node') await refreshNode(); else if (me) await refreshNodes(); } catch (e) { if (e.status === 401) { me = null; show('dashboard', false); show('auth'); $('toolbar').replaceChildren(); } else notice(e.message, true); } finally { refreshBusy = false; }
}, 5000);
boot().catch(e => { show('loading', false); notice(e.message, true); });
