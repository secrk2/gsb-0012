// ========== UI 基础组件 ==========

export const STATUS_LABELS = {
  none: '建档',
  pending: '待入场',
  onsite: '在场',
  leave: '请假离场',
  exited: '退场',
  blacklisted: '黑名单',
};

export const ALERT_TYPE_LABELS = {
  ai_behavior: 'AI行为识别',
  device: '设备监测',
  access: '门禁闸机',
};

// 创建元素：el('div', {class:'x', onclick: fn}, child1, child2)
export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else if (k === 'dataset') Object.assign(node.dataset, v);
    else node.setAttribute(k, v);
  }
  for (const c of children.flat(Infinity)) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return node;
}

export function statusChip(status) {
  return el('span', { class: `chip st-${status}` }, STATUS_LABELS[status] || status);
}

export function fmtTime(iso) {
  if (!iso) return '-';
  const d = new Date(iso);
  if (isNaN(d)) return iso;
  const pad = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function fmtAgo(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return '';
  const s = Math.floor((Date.now() - d.getTime()) / 1000);
  if (s < 60) return '刚刚';
  if (s < 3600) return `${Math.floor(s / 60)}分钟前`;
  if (s < 86400) return `${Math.floor(s / 3600)}小时前`;
  return `${Math.floor(s / 86400)}天前`;
}

// ---------- Toast ----------
export function toast(msg, type = 'ok', ms = 2600) {
  const root = document.getElementById('toast-root');
  const t = el('div', { class: `toast ${type}` }, msg);
  root.append(t);
  setTimeout(() => t.remove(), ms);
}

// ---------- Modal ----------
export function openModal({ title, sub, body, actions = [] }) {
  const root = document.getElementById('modal-root');
  root.innerHTML = '';
  const close = () => { root.innerHTML = ''; };
  const actionBar = el('div', { class: 'modal-actions' },
    actions.map(a => el('button', {
      class: `btn ${a.kind || ''}`,
      onclick: () => a.onClick ? a.onClick(close) : close(),
    }, a.label)),
  );
  const modal = el('div', { class: 'modal' },
    el('h3', {}, title),
    sub ? el('div', { class: 'modal-sub' }, sub) : null,
    body,
    actionBar,
  );
  const mask = el('div', { class: 'modal-mask', onclick: (e) => { if (e.target === mask) close(); } }, modal);
  root.append(mask);
  return close;
}

// ---------- 错误态（越权/不存在/加载失败，绝不给空白页） ----------
export function errorState({ icon = '⛔', title, desc, backHash = '#/dashboard', backLabel = '返回作战台' }) {
  return el('div', { class: 'error-state' },
    el('div', { class: 'icon' }, icon),
    el('h2', {}, title),
    el('p', {}, desc),
    el('a', { class: 'btn', href: backHash }, backLabel),
  );
}

// ---------- 数据时间戳 / 离线陈旧标记 ----------
export function dataTimestamp(cachedAt, stale) {
  if (!cachedAt) return null;
  return el('span', { class: 'data-ts' },
    stale ? el('span', { class: 'stale-badge' }, `离线缓存 · 更新于 ${fmtTime(new Date(cachedAt).toISOString())}`)
          : `数据更新于 ${fmtTime(new Date(cachedAt).toISOString())}`,
  );
}

export function skeletonCard(h = 120) {
  return el('div', { class: 'skeleton', style: `height:${h}px` });
}
