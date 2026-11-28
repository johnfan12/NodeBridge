'use strict';
const $ = id => document.getElementById(id);
const adminPage = document.body.dataset.page === 'admin';
function on(id, event, action) { $(id)?.addEventListener(event, action); }
let config = {}, me = null, nodes = [], refreshBusy = false;
let detailKey = '', historySelection = '';
let announcementsKey = '';
let updatesKey = '';
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

const gpuInterval = 20 * 60 * 1000;
function gpuSnapshot(n) {
  if (n.gpu_snapshot) return { ...n.gpu_snapshot, gpus: n.gpu_snapshot.gpus || [], legacy: !!n.status?.at && (!n.status.gpu_sampled_at || n.status.gpu_sampled_at.startsWith('0001')) };
  if (n.status?.gpu_sampled_at && !n.status.gpu_sampled_at.startsWith('0001')) return { at: n.status.gpu_sampled_at, gpus: n.status.gpus || [], error: n.status.gpu_error, legacy: false };
  if (n.status?.at && !n.status.at.startsWith('0001')) return { at: n.status.at, gpus: n.status.gpus || [], error: n.status.gpu_error, legacy: true };
  return { at: '', gpus: [], error: '', legacy: false };
}
function validMetric(value) { return typeof value === 'number' && Number.isFinite(value) && value >= 0; }
function gpuState(g) {
  if (!validMetric(g.utilization) || g.utilization > 100 || !validMetric(g.memory_used_mb) || !validMetric(g.memory_total_mb) || g.memory_total_mb <= 0 || g.memory_used_mb > g.memory_total_mb) return 'unknown';
  return g.utilization < 10 && g.memory_used_mb < 1024 ? 'available' : 'busy';
}
function gpuSummary(n) {
  const snapshot = gpuSnapshot(n), age = Date.now() - new Date(snapshot.at).getTime();
  const stale = !Number.isFinite(age) || age > gpuInterval + 60 * 1000 || age < -60 * 1000;
  const total = snapshot.gpus.length, available = snapshot.gpus.filter(g => gpuState(g) === 'available').length;
  let text = `可用 ${available}/${total}`;
  if (!snapshot.at) text = '等待快照';
  else if (snapshot.error) text = '显卡未知';
  else if (!total) text = '无显卡';
  else if (!n.online || stale) text = `可用 —/${total}`;
  return { snapshot, stale, total, available, text };
}
function gpuAvailability(n) {
  const summary = gpuSummary(n), badge = el('span', summary.text, 'gpu-availability');
  badge.title = summary.snapshot.at ? `显卡快照：${date(summary.snapshot.at)}${summary.snapshot.legacy ? ' · 旧版节点上报' : ' · 每 20 分钟采集'}${!n.online ? ' · 节点离线' : summary.stale ? ' · 快照已过期' : ''}${summary.snapshot.error ? ` · ${summary.snapshot.error}` : ''}` : '等待节点上报显卡快照';
  return badge;
}
function detailID() {
  if (!location.hash.startsWith('#server=')) return '';
  try { return decodeURIComponent(location.hash.slice(8)); } catch { return ''; }
}
function openNodeDetails(id) { location.hash = `server=${encodeURIComponent(id)}`; }
function syncNodeView(focus = false) {
  if (adminPage || !me) return;
  const id = detailID();
  show('dashboard', !id); show('server-detail', !!id);
  if (id) {
    renderNodeDetails(id);
    if (focus) { $('server-detail-title')?.focus({ preventScroll: true }); window.scrollTo(0, 0); }
  }
}

function gpuMeter(value, label) {
  const meter = el('progress', undefined, 'gpu-meter'); meter.max = 100;
  if (validMetric(value)) meter.value = Math.min(100, value);
  else { meter.value = 0; meter.classList.add('unknown'); }
  meter.setAttribute('aria-label', `${label}：${validMetric(value) ? metric(value, '%') : '未知'}`);
  return meter;
}
function gpuCard(g) {
  const state = gpuState(g), card = el('article', undefined, `gpu-card ${state}`), head = el('div', undefined, 'gpu-card-head');
  const name = el('div'); name.append(el('p', `GPU ${g.index}`, 'eyebrow'), el('h3', g.name));
  head.append(name, el('span', state === 'available' ? '可用' : state === 'busy' ? '占用中' : '数据未知', `gpu-state ${state}`)); card.append(head);
  const util = el('div', undefined, 'gpu-measure'); util.append(el('span', 'GPU 利用率'), el('strong', metric(g.utilization, '%')));
  card.append(util, gpuMeter(g.utilization, 'GPU 利用率'));
  const memory = el('div', undefined, 'gpu-measure');
  const memoryText = `${validMetric(g.memory_used_mb) ? (g.memory_used_mb / 1024).toFixed(1) : '—'} / ${validMetric(g.memory_total_mb) ? (g.memory_total_mb / 1024).toFixed(1) : '—'} GiB`;
  memory.append(el('span', '显存使用'), el('strong', memoryText));
  const percent = validMetric(g.memory_used_mb) && validMetric(g.memory_total_mb) && g.memory_total_mb > 0 ? g.memory_used_mb / g.memory_total_mb * 100 : null;
  card.append(memory, gpuMeter(percent, '显存占用率'));
  const environment = el('div', undefined, 'gpu-environment');
  environment.append(el('span', `温度 ${metric(g.temperature, '°C')}`), el('span', `功耗 ${metric(g.power_watts, ' W')}`)); card.append(environment);
  return card;
}
function renderGPUs(n) {
  const { snapshot, stale, available, total } = gpuSummary(n), section = el('section', undefined, 'workspace detail-section');
  const head = el('div', undefined, 'workspace-head'), intro = el('div');
  intro.append(el('h2', '显卡占用快照'), el('p', snapshot.at ? `采集于 ${date(snapshot.at)}${snapshot.legacy ? ' · 旧版节点上报' : ' · 每 20 分钟采集'}` : '等待节点首次采集', 'muted'));
  head.append(intro); section.append(head);
  const body = el('div', undefined, 'workspace-body');
  if (!n.online) body.append(el('p', '节点已离线，以下为最后一次上报的数据。', 'snapshot-warning'));
  else if (snapshot.at && stale) body.append(el('p', '快照已过期，等待节点下次采集。以下数据仅供参考。', 'snapshot-warning'));
  if (snapshot.legacy) body.append(el('p', '此节点尚未启用快照采集；升级节点至 v0.6.0 后，每 20 分钟采集一次。', 'snapshot-warning'));
  if (snapshot.error) body.append(el('p', snapshot.error, 'snapshot-warning'));
  if (total && !snapshot.error) {
    const summary = el('div', undefined, 'snapshot-summary');
    const counts = [[`${available}`, '可用'], [`${snapshot.gpus.filter(g => gpuState(g) === 'busy').length}`, '占用中'], [`${snapshot.gpus.filter(g => gpuState(g) === 'unknown').length}`, '未知']];
    for (const [value, label] of counts) { const count = el('div'); count.append(el('strong', value), el('span', label)); summary.append(count); }
    body.append(summary);
    const grid = el('div', undefined, 'gpu-grid'); for (const g of snapshot.gpus) grid.append(gpuCard(g)); body.append(grid);
  } else if (!snapshot.error) body.append(el('p', snapshot.at ? '本次快照未检测到 NVIDIA 显卡。' : '节点连接后将自动采集首个显卡快照。', 'detail-empty'));
  body.append(el('p', '可用口径：利用率低于 10%，且已用显存低于 1 GiB。按快照估算，不代表资源预留；未知数据不计为可用。', 'snapshot-caption'));
  section.append(body); return section;
}
function historyPercent(day, key) { return day.checks > 0 ? Math.min(100, Math.max(0, day[key] / day.checks * 100)) : null; }
function historyText(day) {
  return `${day.date}（UTC） · ${day.checks ? `在线 ${historyPercent(day, 'online').toFixed(1)}% · SSH 就绪 ${historyPercent(day, 'ssh_ready').toFixed(1)}% · ${day.checks} 次采样` : '未采样'}`;
}
function renderHistory(n) {
  const section = el('section', undefined, 'workspace detail-section'), head = el('div', undefined, 'workspace-head'), intro = el('div');
  intro.append(el('h2', '过去 30 天可用性'), el('p', '按 UTC 日期汇总，每 30 秒采样', 'muted')); head.append(intro); section.append(head);
  const body = el('div', undefined, 'workspace-body'), days = n.history || [], checks = days.reduce((sum, day) => sum + day.checks, 0);
  const summary = el('div', undefined, 'history-summary');
  for (const [key, label] of [['online', '节点在线率'], ['ssh_ready', 'SSH 就绪率']]) {
    const value = checks ? `${(days.reduce((sum, day) => sum + Math.min(day.checks, Math.max(0, day[key])), 0) / checks * 100).toFixed(1)}%` : '—';
    const item = el('div'); item.append(el('span', label), el('strong', value)); summary.append(item);
  }
  const sampled = el('div'); sampled.append(el('span', '有采样记录'), el('strong', `${days.filter(day => day.checks > 0).length} / ${days.length} 天`)); summary.append(sampled); body.append(summary);
  const note = el('p', undefined, 'history-note'); note.setAttribute('aria-live', 'polite');
  const dateLabel = el('label', '查看日期', 'history-date-control'), dateSelect = el('select'); dateSelect.setAttribute('aria-label', '查看日期');
  for (const day of [...days].reverse()) { const option = el('option', day.date); option.value = day.date; dateSelect.append(option); }
  dateSelect.disabled = !days.length; dateLabel.append(dateSelect);
  const select = day => {
    historySelection = day.date; note.textContent = historyText(day); dateSelect.value = day.date;
    for (const cell of body.querySelectorAll('.history-day')) cell.setAttribute('aria-pressed', String(cell.dataset.date === day.date));
  };
  dateSelect.addEventListener('change', () => { const day = days.find(day => day.date === dateSelect.value); if (day) select(day); });
  const timeline = el('div', undefined, 'history-timeline');
  for (const [key, label] of [['online', '节点在线'], ['ssh_ready', 'SSH 就绪']]) {
    const row = el('div', undefined, 'history-row'), strip = el('div', undefined, 'history-strip'); strip.setAttribute('role', 'group'); strip.setAttribute('aria-label', `${label}每日记录`);
    row.append(el('span', label, 'history-label'));
    for (const day of days) {
      const percent = historyPercent(day, key), cell = el('button', undefined, `history-day ${percent === null ? 'unknown' : percent >= 95 ? 'good' : percent > 0 ? 'partial' : 'bad'}`);
      cell.type = 'button'; cell.dataset.date = day.date; cell.dataset.metric = key;
      cell.title = historyText(day); cell.setAttribute('aria-label', `${day.date} ${label} ${percent === null ? '未采样' : percent.toFixed(1) + '%'}`);
      cell.addEventListener('click', () => select(day)); cell.addEventListener('focus', () => select(day)); strip.append(cell);
    }
    row.append(strip); timeline.append(row);
  }
  if (days.length) {
    const axis = el('div', undefined, 'history-axis'); axis.append(el('span', days[0].date), el('span', days[days.length - 1].date)); timeline.append(axis);
  }
  body.append(timeline, dateLabel, note);
  const legend = el('div', undefined, 'history-legend');
  for (const [cls, label] of [['good', '≥95%'], ['partial', '部分可用'], ['bad', '0%'], ['unknown', '未采样']]) { const item = el('span', label); item.prepend(el('i', undefined, cls)); legend.append(item); }
  body.append(legend, el('p', '点击或用键盘选中日期可查看当日详情。仅统计控制台运行期间的观察结果，未采样不等于离线。', 'snapshot-caption'));
  if (!checks) body.append(el('p', '尚无历史采样，连接后会自动积累记录。', 'detail-empty'));
  const selected = days.find(day => day.date === historySelection) || [...days].reverse().find(day => day.checks) || days[days.length - 1];
  if (selected) select(selected);
  section.append(body); return section;
}
function renderNodeDetails(id) {
  const n = nodes.find(node => node.id === id), content = $('server-detail-content');
  if (!n) {
    detailKey = ''; content.replaceChildren(el('h1', '服务器已移除'), el('p', '返回列表选择其他服务器。', 'muted')); return;
  }
  const { snapshot, stale } = gpuSummary(n);
  const key = JSON.stringify([n.id, n.name, n.host, n.port, n.version, n.online, n.ssh_ready, n.hub_paused, n.forwarding_paused, n.node_paused, n.error, n.status?.hostname, n.status?.cpus, n.arch, snapshot.legacy ? snapshot.gpus : snapshot, stale, n.history]);
  if (key === detailKey) return;
  const focused = content.contains(document.activeElement) ? document.activeElement : null;
  const focusDay = focused?.dataset.date, focusMetric = focused?.dataset.metric;
  const intro = el('div', undefined, 'detail-intro'), info = el('div'), title = el('h1', n.name), status = nodeState(n);
  title.id = 'server-detail-title'; title.tabIndex = -1;
  info.append(el('p', '服务器详情', 'eyebrow'), title, el('p', `${n.host}:${n.port}${n.version ? ` · ${n.version}` : ''}${n.status?.hostname ? ` · ${n.status.hostname}` : ''}${n.status?.cpus ? ` · ${n.status.cpus} 核` : ''}`, 'node-address'));
  const actions = el('div', undefined, 'actions'); actions.append(el('span', status.text, `badge ${status.cls}`));
  const connect = button('SSH 连接', () => openSSH(n.id), 'primary'); connect.disabled = !n.ssh_ready; connect.dataset.detailConnect = 'true'; actions.append(connect);
  intro.append(info, actions); content.replaceChildren(intro);
  if (n.error) content.append(el('p', n.error, 'snapshot-warning'));
  content.append(renderGPUs(n), renderHistory(n)); detailKey = key;
  if (focusDay) [...content.querySelectorAll('.history-day')].find(cell => cell.dataset.date === focusDay && cell.dataset.metric === focusMetric)?.focus({ preventScroll: true });
  else if (focused?.closest('.history-date-control')) content.querySelector('.history-date-control select')?.focus({ preventScroll: true });
  else if (focused?.dataset.detailConnect) connect.focus({ preventScroll: true });
}

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
  if (adminPage) { activateTab(location.hash.slice(1)); await Promise.all([refreshAdminNodes(), refreshUsers(), refreshAudit(), refreshAnnouncements(), refreshUpdates()]); } else await Promise.all([refreshNodes(), refreshAnnouncements()]);
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
  return { id: row.dataset.nodeId, index: [...row.querySelectorAll('button, summary, a')].indexOf(active) };
}
function restoreListFocus(content, focus) {
  if (!focus || document.activeElement !== document.body) return;
  const row = [...content.querySelectorAll('[data-node-id]')].find(row => row.dataset.nodeId === focus.id);
  row?.querySelectorAll('button, summary, a')[focus.index]?.focus({ preventScroll: true });
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
    info.append(title, el('p', `${n.host}:${n.port}${n.version ? ` · ${n.version}` : ''}`, 'node-address'));
    const actions = el('div', undefined, 'actions node-actions'), operations = el('div', undefined, 'actions node-operations'), overview = el('div', undefined, 'actions node-overview');
    const connect = button('SSH 连接', () => openSSH(n.id), 'primary'); connect.disabled = !n.ssh_ready;
    operations.append(connect); if (me.admin) operations.append(nodePauseButton(n, refreshNodes));
    overview.append(gpuAvailability(n), button('详情', () => openNodeDetails(n.id), 'secondary detail-button'));
    actions.append(operations, overview);
    head.append(info, actions); row.append(head);
    if (n.hub_paused || n.forwarding_paused || n.node_paused) row.append(el('p', `暂停来源：${[n.hub_paused && 'VPS 全局', n.forwarding_paused && 'VPS 节点设置', n.node_paused && '节点本机'].filter(Boolean).join('、')}`, 'muted'));
    if (n.error) row.append(el('p', n.error, 'muted'));
    if (!n.online && n.last_seen && !n.last_seen.startsWith('0001')) row.append(el('p', `最后在线 ${date(n.last_seen)}`, 'muted'));
    content.append(row);
  }
  restoreListFocus(content, focus);
  syncNodeView();
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
    info.append(title, el('p', `${n.host}:${n.port}${n.version ? ` · ${n.version}` : ''}`, 'node-address'));
    const actions = el('div', undefined, 'actions');
    actions.append(nodePauseButton(n, refreshAdminNodes), button('移除节点', async () => {
      if (prompt(`输入「${n.name}」确认移除。配对凭证将撤销，SSH 会话将断开。`) !== n.name) return;
      await api(`/api/nodes/${encodeURIComponent(n.id)}`, 'DELETE', {}); notice('节点已移除'); await refreshAdminNodes();
    }, 'secondary danger destructive-action'));
    const detailLink = el('a', '详情', 'toolbar-link detail-button'); detailLink.href = `/#server=${encodeURIComponent(n.id)}`;
    actions.append(gpuAvailability(n), detailLink);
    row.append(info, actions); content.append(row);
  }
  restoreListFocus(content, focus);
}
async function refreshNode() {
  const s = await api('/api/node'); const content = $('local-status'); content.replaceChildren();
  content.append(el('span', s.online ? '已连接' : s.paired ? '正在连接控制台' : '等待配对', `badge ${s.online ? 'good' : ''}`));
  if (s.hub_url) content.append(el('p', s.hub_url, 'muted'));
  content.append(el('p', `本地 SSH 端口：${s.ssh_port}${s.version ? ` · NodeBridge ${s.version}` : ''}`, 'muted'));
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
function announcementArticle(item) {
  const article = el('article', undefined, 'announcement');
  article.append(el('h3', item.title), el('p', item.content, 'announcement-content'), el('p', `${item.author} · ${date(item.created)}`, 'announcement-meta'));
  return article;
}
async function refreshAnnouncements() {
  const items = await api('/api/announcements');
  if (!me) return;
  const key = JSON.stringify([me.admin, items]);
  if (key === announcementsKey) return;
  if (adminPage) {
    const content = $('admin-announcements'), focusedID = document.activeElement.closest('[data-announcement-id]')?.dataset.announcementId;
    content.replaceChildren(); $('announcement-count').textContent = `${items.length} / 20 条`;
    if (!items.length) {
      const empty = el('div', undefined, 'empty-state');
      empty.append(el('h3', '还没有公告'), el('p', '发布维护通知或使用说明，让用户在首页看到。', 'muted')); content.append(empty);
    }
    for (const item of items) {
      const article = announcementArticle(item); article.classList.add('admin-announcement'); article.dataset.announcementId = item.id;
      const actions = el('div', undefined, 'actions');
      const withdraw = button('撤下公告', async () => {
        if (!confirm(`撤下「${item.title}」？用户首页将不再显示这条公告。`)) return;
        await api(`/api/announcements/${encodeURIComponent(item.id)}`, 'DELETE', {});
        notice('公告已撤下'); await Promise.all([refreshAnnouncements(), refreshAudit()]);
      }, 'secondary danger');
      withdraw.setAttribute('aria-label', `撤下公告：${item.title}`); actions.append(withdraw); article.append(actions); content.append(article);
    }
    if (focusedID && document.activeElement === document.body) {
      [...content.children].find(article => article.dataset.announcementId === focusedID)?.querySelector('button')?.focus({ preventScroll: true });
    }
  } else {
    show('announcement-board', items.length > 0); show('announcement-manage', me.admin);
    $('announcement-latest').replaceChildren(); $('announcement-older').replaceChildren();
    if (items.length) $('announcement-latest').append(announcementArticle(items[0]));
    show('announcement-history', items.length > 1);
    if (items.length <= 1) $('announcement-history').open = false;
    $('announcement-history-title').textContent = `较早公告（${Math.max(0, items.length - 1)}）`;
    for (const item of items.slice(1)) $('announcement-older').append(announcementArticle(item));
  }
  announcementsKey = key;
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
const updatePhases = { queued: '等待执行', checking: '检查发布文件', downloading: '正在下载安装包', verifying: '校验安装包', installing: '正在安装', restarting: '正在重启', rolling_back: '正在恢复旧版本', rolled_back: '已恢复旧版本', succeeded: '升级完成', failed: '升级失败' };
function updateBusy(phase) { return ['queued', 'checking', 'downloading', 'verifying', 'installing', 'restarting', 'rolling_back'].includes(phase); }
function newerVersion(a, b) {
  const pattern = /^v(0|[1-9]\d{0,5})\.(0|[1-9]\d{0,5})\.(0|[1-9]\d{0,5})$/;
  const x = pattern.exec(a || ''), y = pattern.exec(b || '');
  if (!x || !y) return false;
  for (let i = 1; i <= 3; i++) if (+x[i] !== +y[i]) return +x[i] > +y[i];
  return false;
}
function updateRow(item, latest, hub = false) {
  const row = el('article', undefined, 'update-row'), info = el('div', undefined, 'node-info'); row.dataset.nodeId = hub ? 'hub' : item.id;
  const title = el('div', undefined, 'node-title'); title.append(el('h3', hub ? '控制台 Hub' : item.name));
  if (!hub) title.append(el('span', item.online ? '在线' : '离线', `badge ${item.online ? 'good' : ''}`));
  info.append(title, el('p', `${item.version || '旧版 · 未上报版本'}${item.arch ? ` · ${item.arch}` : ''}`, 'node-address'));
  if (latest.version === item.version) info.append(el('p', '已是最新正式版本', 'muted'));
  const job = item.update || {}, busy = updateBusy(job.phase);
  if (job.phase) info.append(el('p', `${updatePhases[job.phase] || job.phase}${job.version ? ` · ${job.version}` : ''}${job.message ? `：${job.message}` : ''}`, job.phase === 'failed' || job.phase === 'rolled_back' ? 'update-error' : 'muted'));
  if (!item.version) info.append(el('p', '版本尚未上报；旧版节点需先运行一次新版安装命令。', 'muted'));
  else if (!item.updater_ready && (hub || item.online)) info.append(el('p', item.updater_issue || '执行器未就绪，请检查更新监听器状态和更新服务日志。', 'muted'));
  else if (!hub && !item.online) info.append(el('p', '节点上线后可执行升级。', 'muted'));
  const canUpdate = item.updater_ready && (hub || item.online) && newerVersion(latest.version, item.version) && !busy;
  const action = button(busy ? '升级进行中…' : '升级', async () => {
    const scope = hub ? '此 VPS 上的 NodeBridge 服务' : `节点「${item.name}」`;
    if (!confirm(`将${scope}从 ${item.version} 更新到 ${latest.version}？安装时会重启服务并断开对应 SSH 会话。`)) return;
    await api(hub ? '/api/updates/hub' : `/api/nodes/${encodeURIComponent(item.id)}/update`, 'POST', { version: latest.version });
    $('update-feedback').textContent = '升级任务已提交，进度会自动更新。'; await refreshUpdates();
  }, canUpdate ? 'primary' : 'secondary');
  action.disabled = !canUpdate; action.setAttribute('aria-label', `升级${hub ? '控制台 Hub' : item.name}`);
  row.append(info, action); return row;
}
async function refreshUpdates() {
  const data = await api('/api/updates'), key = JSON.stringify(data);
  if (!me || key === updatesKey) return;
  const latest = data.latest || {}, content = $('update-nodes'), focus = listFocus(content);
  $('update-latest').replaceChildren();
  if (latest.version) {
    const link = el('a', latest.version); link.href = latest.url; link.target = '_blank'; link.rel = 'noopener noreferrer';
    $('update-latest').append(el('span', '最新正式版 '), link, el('span', ` · 检查于 ${date(latest.checked_at)}`));
  } else $('update-latest').textContent = '点击“检查更新”获取最新正式版本。';
  const hubFocus = listFocus($('update-hub')); $('update-hub').replaceChildren(updateRow(data.hub, latest, true)); restoreListFocus($('update-hub'), hubFocus);
  content.replaceChildren(); $('update-node-count').textContent = `${data.nodes.length} 台服务器`;
  for (const item of data.nodes) content.append(updateRow(item, latest));
  if (!data.nodes.length) content.append(el('p', '接入节点后，会在这里显示版本和升级状态。', 'workspace-body muted'));
  restoreListFocus(content, focus); updatesKey = key;
  if (data.hub.update?.phase === 'succeeded' && data.hub.version !== config.version) {
    $('update-feedback').replaceChildren(el('span', '控制台已升级。'), button('刷新页面', () => location.reload()));
  }
}
on('check-update', 'click', async e => {
  e.target.disabled = true; $('update-feedback').textContent = '正在检查 GitHub 正式发布版本…';
  try { await api('/api/updates/check', 'POST', {}); $('update-feedback').textContent = '版本检查完成，请选择需要升级的服务。'; await refreshUpdates(); }
  catch (err) { $('update-feedback').textContent = err.message; }
  finally { e.target.disabled = false; }
});
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
form('announcement-form', async (data, f) => {
  await api('/api/announcements', 'POST', { title: data.get('title').trim(), content: data.get('content').trim() });
  f.reset(); notice('公告已发布，用户首页将自动更新'); await Promise.all([refreshAnnouncements(), refreshAudit()]);
});
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
window.addEventListener('hashchange', () => { if (adminPage) activateTab(location.hash.slice(1)); else syncNodeView(true); });
on('server-detail-back', 'click', () => {
  const id = detailID(); location.hash = ''; syncNodeView();
  [...$('nodes').querySelectorAll('[data-node-id]')].find(row => row.dataset.nodeId === id)?.querySelector('.detail-button')?.focus({ preventScroll: true });
});
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
  try { if (config.mode === 'node') await refreshNode(); else if (me) { await Promise.all([adminPage ? refreshAdminNodes() : refreshNodes(), refreshAnnouncements(), ...(adminPage ? [refreshUpdates()] : [])]); } } catch (e) { if (e.status === 401) { me = null; $('ssh-dialog')?.close(); show('dashboard', false); show('server-detail', false); show('admin-panel', false); show('auth'); $('toolbar').replaceChildren(); } else if (adminPage && $('tab-updates')?.getAttribute('aria-selected') === 'true') { $('update-feedback').textContent = '暂时无法连接控制台；升级重启期间会自动重试。'; } else notice(e.message, true); } finally { refreshBusy = false; }
}, 5000);
boot().catch(e => { show('loading', false); notice(e.message, true); });
