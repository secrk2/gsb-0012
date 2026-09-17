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

// 恢复在线后批量同步：服务端按 client_op_id 幂等去重，冲突返回当前状态
export async function flushOutbox() {
  const ops = getOutbox();
  if (!ops.length || !online) return { flushed: 0, conflicts: [] };
  const transitions = ops.filter(o => o.type === 'transition');
  if (!transitions.length) return { flushed: 0, conflicts: [] };
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
    const doneIds = new Set(results.map(r => r.client_op_id));
    saveOutbox(ops.filter(o => !doneIds.has(o.client_op_id)));
    return {
      flushed: results.filter(r => r.status === 'applied').length,
      duplicates: results.filter(r => r.status === 'duplicate').length,
      conflicts: results.filter(r => r.status === 'conflict' || r.status === 'error'),
    };
  } catch (e) {
    return { flushed: 0, conflicts: [], error: e };
  }
}
