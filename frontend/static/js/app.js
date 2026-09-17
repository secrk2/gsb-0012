// ========== 工瞳 SPA：路由 + 应用壳 + 离线横幅 ==========
import { getToken, getUser, clearSession, api, isOnline, onConnectivityChange, pendingOpCount, flushOutbox } from './api.js';
import { el, toast } from './ui.js';
import { renderLogin } from './views/login.js';
import { renderDashboard } from './views/dashboard.js';
import { renderWorkers } from './views/workers.js';
import { renderWorkerDetail } from './views/workerDetail.js';
import { renderAttendance } from './views/attendance.js';
import { renderPayroll } from './views/payroll.js';

const app = document.getElementById('app');

function parseHash() {
  const h = (location.hash || '#/').slice(1);
  const [path, qs] = h.split('?');
  return { path, params: new URLSearchParams(qs || '') };
}

// ---------- 离线横幅 ----------
function refreshBanner() {
  const banner = document.getElementById('offline-banner');
  const pending = pendingOpCount();
  if (!isOnline()) {
    banner.className = 'offline-banner';
    banner.innerHTML = '';
    banner.append(
      el('span', { class: 'dot' }),
      `当前离线：展示的是缓存数据，请勿据此做现场决策${pending ? `；${pending} 条变更已排队，恢复后自动同步` : ''}`,
    );
    document.body.classList.add('has-offline-banner');
  } else if (pending > 0) {
    banner.className = 'offline-banner';
    banner.innerHTML = '';
    banner.append(el('span', { class: 'dot' }), `正在同步 ${pending} 条离线变更…`);
    document.body.classList.add('has-offline-banner');
  } else {
    banner.className = 'offline-banner hidden';
    document.body.classList.remove('has-offline-banner');
  }
}

onConnectivityChange(async (online) => {
  refreshBanner();
  if (online) {
    const res = await flushOutbox();
    refreshBanner();
    if (res.flushed > 0 || res.duplicates > 0) {
      toast(`离线变更已同步 ${res.flushed} 条${res.duplicates ? `，去重 ${res.duplicates} 条` : ''}`, 'ok');
    }
    if (res.conflicts && res.conflicts.length) {
      toast(`${res.conflicts.length} 条变更与服务器状态冲突，已按服务器最新状态合并，请刷新查看`, 'warn', 4200);
    }
    route(); // 同步后刷新当前页数据
  }
});

// ---------- 应用壳 ----------
function shell(content, activeNav) {
  const user = getUser();
  const isWorker = user.role === 'worker';
  const navItems = isWorker
    ? [
        { hash: '#/workers/me', label: '我的档案', ico: '🪪' },
        { hash: '#/attendance', label: '出勤打卡', ico: '🕒' },
        { hash: '#/payroll', label: '我的工资', ico: '💰' },
      ]
    : [
        { hash: '#/dashboard', label: '工地作战台', ico: '📊' },
        { hash: '#/attendance', label: '出勤打卡', ico: '🕒' },
        { hash: '#/payroll', label: '工资分账', ico: '💰' },
        { hash: '#/workers', label: '人员档案', ico: '👷' },
      ];

  const topNav = el('nav', {},
    navItems.map(n => el('a', { href: n.hash, class: activeNav === n.hash ? 'active' : '' }, n.label)),
  );
  const bottomNav = el('nav', { class: 'bottomnav' },
    navItems.map(n => el('a', { href: n.hash, class: activeNav === n.hash ? 'active' : '' },
      el('span', { class: 'ico' }, n.ico), n.label)),
  );

  return el('div', {},
    el('header', { class: 'topbar' },
      el('div', { class: 'brand' }, el('span', { class: 'logo' }, '◉'), '工瞳'),
      topNav,
      el('div', { class: 'user-chip' },
        el('span', { class: 'role-tag' }, user.role_label || user.role),
        el('span', { class: 'uname' }, user.name),
        el('button', {
          class: 'logout-btn',
          onclick: async () => {
            try { await api.post('/api/logout'); } catch {}
            clearSession();
            location.hash = '#/login';
          },
        }, '退出'),
      ),
    ),
    el('main', { class: 'main' }, content),
    bottomNav,
  );
}

// ---------- 路由 ----------
async function route() {
  const { path, params } = parseHash();
  const token = getToken();
  const user = getUser();

  if (path === '/login') {
    app.innerHTML = '';
    app.append(renderLogin());
    return;
  }
  if (!token || !user) {
    location.hash = '#/login';
    return;
  }

  let content;
  let activeNav = '#/dashboard';

  if (path === '/' || path === '/dashboard') {
    content = el('div', {});
    app.innerHTML = '';
    app.append(shell(content, '#/dashboard'));
    renderDashboard(content, params);
    return;
  }
  if (path === '/workers') {
    if (user.role === 'worker') { location.hash = '#/workers/me'; return; }
    content = el('div', {});
    app.innerHTML = '';
    app.append(shell(content, '#/workers'));
    renderWorkers(content, params);
    return;
  }
  if (path === '/attendance') {
    content = el('div', {});
    app.innerHTML = '';
    app.append(shell(content, '#/attendance'));
    renderAttendance(content, params);
    return;
  }
  if (path === '/payroll') {
    content = el('div', {});
    app.innerHTML = '';
    app.append(shell(content, '#/payroll'));
    renderPayroll(content, params);
    return;
  }
  const m = path.match(/^\/workers\/(\w+)$/);
  if (m) {
    activeNav = user.role === 'worker' ? '#/workers/me' : '#/workers';
    content = el('div', {});
    app.innerHTML = '';
    app.append(shell(content, activeNav));
    renderWorkerDetail(content, m[1]);
    return;
  }
  // 未知路径
  location.hash = '#/dashboard';
}

window.addEventListener('hashchange', route);
refreshBanner();
route();
