// ========== 工瞳 API 客户端：鉴权 + 离线检测 + 缓存 + 变更队列 ==========

const TOKEN_KEY = 'gt_token';
const USER_KEY = 'gt_user';
const OUTBOX_KEY = 'gt_outbox';
const CACHE_PREFIX = 'gt_cache:';

export function getToken() { return localStorage.getItem(TOKEN_KEY) || ''; }
export function getUser() {
  try { return JSON.parse(localStorage.getItem(USER_KEY)); } catch { return null; }
}
export function setSession(token, user) {
  localStorage.setItem(TOKEN_KEY, token);
  localStorage.setItem(USER_KEY, JSON.stringify(user));
}
export function clearSession() {
  localStorage.removeItem(TOKEN_KEY);
  localStorage.removeItem(USER_KEY);
}

// ---------- 在线状态 ----------
let online = navigator.onLine;
const connListeners = new Set();

export function isOnline() { return online; }
export function onConnectivityChange(fn) { connListeners.add(fn); }

function setOnline(v) {
  if (online === v) return;
  online = v;
  connListeners.forEach(fn => fn(v));
}

window.addEventListener('online', () => setOnline(true));
window.addEventListener('offline', () => setOnline(false));

// ---------- 基础请求 ----------
async function request(path, { method = 'GET', body } = {}) {
  const headers = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  let res;
  try {
    res = await fetch(path, { method, headers, body: body ? JSON.stringify(body) : undefined });
  } catch (e) {
    setOnline(false);
    const err = new Error('网络连接失败，当前处于离线状态');
    err.offline = true;
    throw err;
  }
  setOnline(true);
  const data = await res.json().catch(() => ({}));
  if (res.status === 401) {
    clearSession();
    if (location.hash !== '#/login') location.hash = '#/login';
    const err = new Error(data.error || '登录已过期');
    err.status = 401;
    throw err;
  }
  if (!res.ok) {
    const err = new Error(data.error || `请求失败（${res.status}）`);
    err.status = res.status;
    err.data = data;
    throw err;
  }
  return data;
}

export const api = {
  get: (p) => request(p),
  post: (p, body) => request(p, { method: 'POST', body }),
};

// ---------- 读缓存：离线时回退到缓存并标注时间戳，绝不拿旧数据冒充新数据 ----------
export async function cachedGet(key, path) {
  try {
    const data = await api.get(path);
    const at = Date.now();
    try { localStorage.setItem(CACHE_PREFIX + key, JSON.stringify({ data, at })); } catch {}
    return { data, cachedAt: at, stale: false };
  } catch (e) {
    const raw = localStorage.getItem(CACHE_PREFIX + key);
    if (raw && (e.offline || e.status >= 500)) {
      const { data, at } = JSON.parse(raw);
      return { data, cachedAt: at, stale: true, error: e };
    }
    throw e;
  }
}

// ---------- 离线变更队列 ----------
export function getOutbox() {
  try { return JSON.parse(localStorage.getItem(OUTBOX_KEY)) || []; } catch { return []; }
}

// 非安全上下文（如 http://局域网IP）没有 crypto.randomUUID，降级生成
export function newOpId() {
  if (window.crypto && typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  return 'op-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 10);
}
function saveOutbox(ops) {
  localStorage.setItem(OUTBOX_KEY, JSON.stringify(ops));
  connListeners.forEach(fn => fn(online)); // 触发横幅刷新
}
export function pendingOpCount() { return getOutbox().length; }

// 状态变更：在线直接提交；离线/失败入队，带 client_op_id 保证幂等
export async function submitTransition(workerId, payload) {
  const op = {
    type: 'transition',
    worker_id: workerId,
    to: payload.to,
    reason: payload.reason || '',
    face_verified: !!payload.face_verified,
    id_verified: !!payload.id_verified,
    client_op_id: payload.client_op_id || newOpId(),
    client_time: new Date().toISOString(),
  };
  if (!online) {
    const ops = getOutbox(); ops.push(op); saveOutbox(ops);
    return { queued: true, client_op_id: op.client_op_id };
  }
  try {
    return await api.post(`/api/workers/${workerId}/transition`, op);
  } catch (e) {
    if (e.offline) {
      const ops = getOutbox(); ops.push(op); saveOutbox(ops);
      return { queued: true, client_op_id: op.client_op_id };
    }
    throw e;
  }
}

// 出勤打卡（闸机/手机定位）：同样支持离线排队，按 client_op_id 幂等；
// 已结算冻结月补打需 confirm + reason（由调用方在 409 need_confirm 时补齐）。
export async function submitPunch(payload) {
  const op = {
    type: 'punch',
    worker_id: payload.worker_id,
    punch_date: payload.punch_date,
    punch_time: payload.punch_time,
    source: payload.source || 'mobile',
    device: payload.device || '',
    reason: payload.reason || '',
    confirm: !!payload.confirm,
    client_op_id: payload.client_op_id || newOpId(),
    client_time: new Date().toISOString(),
  };
  const post = async () => api.post('/api/attendance/punch', op);
  if (!online) {
    const ops = getOutbox(); ops.push(op); saveOutbox(ops);
    return { queued: true, client_op_id: op.client_op_id };
  }
  try {
    return await post();
  } catch (e) {
    if (e.offline) {
      const ops = getOutbox(); ops.push(op); saveOutbox(ops);
      return { queued: true, client_op_id: op.client_op_id };
    }
    throw e;
  }
}

// 带鉴权头的文件下载（CSV 导出需要 Bearer，不能直接用 <a>）
export async function downloadWithAuth(path, fallbackName) {
  const res = await fetch(path, { headers: { Authorization: 'Bearer ' + getToken() } });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.error || `导出失败（${res.status}）`);
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = res.headers.get('Content-Disposition')?.split('filename="')[1]?.replace('"', '') || fallbackName;
  document.body.append(a); a.click(); a.remove();
  URL.revokeObjectURL(url);
}

// 恢复在线后批量同步：状态变更走批量幂等；打卡逐条重放（同 client_op_id 去重）
export async function flushOutbox() {
  const ops = getOutbox();
  if (!ops.length || !online) return { flushed: 0, conflicts: [] };
  const transitions = ops.filter(o => o.type === 'transition');
  const punches = ops.filter(o => o.type === 'punch');
  let flushed = 0, duplicates = 0;
  const conflicts = [];
  const done = new Set();

  // 打卡逐条（冻结月离线补打的 confirm/reason 已在入队时带上）
  for (const p of punches) {
    try {
      const r = await api.post('/api/attendance/punch', p);
      done.add(p.client_op_id);
      if (r.duplicate) duplicates++; else flushed++;
    } catch (e) {
      if (e.status === 409 && e.data && e.data.need_confirm) conflicts.push({ op: p, error: e.data.error });
      // 其余错误（403/400）保留在队列无意义，直接丢弃并暴露
      if (e.status === 400 || e.status === 403) { done.add(p.client_op_id); conflicts.push({ op: p, error: e.message }); }
    }
  }

  if (transitions.length) {
    try {
      const res = await api.post('/api/sync/batch', {
        ops: transitions.map(t => ({
          client_op_id: t.client_op_id,
          worker_id: t.worker_id,
          to: t.to,
          reason: t.reason,
          client_time: t.client_time,
          face_verified: t.face_verified,
          id_verified: t.id_verified,
        })),
      });
      const results = res.results || [];
      results.forEach(r => done.add(r.client_op_id));
      flushed += results.filter(r => r.status === 'applied').length;
      duplicates += results.filter(r => r.status === 'duplicate').length;
      results.filter(r => r.status === 'conflict' || r.status === 'error').forEach(r => conflicts.push(r));
    } catch (e) {
      return { flushed, conflicts, error: e };
    }
  }

  saveOutbox(ops.filter(o => !done.has(o.client_op_id)));
  return { flushed, duplicates, conflicts };
}
