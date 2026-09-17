// ========== 登录页 ==========
import { api, setSession } from '../api.js';
import { el } from '../ui.js';

const DEMO_ACCOUNTS = [
  { u: 'gc_admin', role: '总包管理员', name: '王建国 · 城东安置房三期' },
  { u: 'sub_leader', role: '分包班组长', name: '李铁军 · 钢筋一班' },
  { u: 'supervisor', role: '监理', name: '陈明远 · 城东安置房三期' },
  { u: 'worker01', role: '工人', name: '张伟 · 钢筋工' },
  { u: 'regulator', role: '监管员', name: '赵正 · 住建局' },
  { u: 'gc_admin2', role: '总包管理员', name: '刘志远 · 滨江商业综合体' },
];

export function renderLogin() {
  const userInput = el('input', { type: 'text', placeholder: '请输入账号', autocomplete: 'username' });
  const passInput = el('input', { type: 'password', placeholder: '请输入密码', autocomplete: 'current-password' });
  const errBox = el('div', { class: 'login-error' });
  const loginBtn = el('button', { class: 'btn primary block' }, '登 录');

  async function doLogin() {
    errBox.textContent = '';
    loginBtn.disabled = true;
    loginBtn.textContent = '登录中…';
    try {
      const res = await api.post('/api/login', {
        username: userInput.value.trim(),
        password: passInput.value,
      });
      setSession(res.token, res.user);
      location.hash = res.user.role === 'worker' ? '#/workers/me' : '#/dashboard';
    } catch (e) {
      errBox.textContent = e.message || '登录失败';
    } finally {
      loginBtn.disabled = false;
      loginBtn.textContent = '登 录';
    }
  }
  loginBtn.addEventListener('click', doLogin);
  passInput.addEventListener('keydown', e => { if (e.key === 'Enter') doLogin(); });

  return el('div', { class: 'login-wrap' },
    el('div', { class: 'login-panel' },
      el('div', { class: 'login-hero' },
        el('h1', {}, el('span', { class: 'eye' }, '◉ '), '工瞳'),
        el('p', {}, '工地人员实名制管理与不安全行为预警平台'),
        el('div', { class: 'feat' },
          el('div', {}, '· 实名制入场：刷脸 + 身份证双核验'),
          el('div', {}, '· 人员状态机：待入场 / 在场 / 请假离场 / 退场 / 黑名单'),
          el('div', {}, '· 姓名脱敏展示，查看全名二次确认并留痕'),
          el('div', {}, '· 离线作业：变更排队，恢复后幂等同步'),
        ),
      ),
      el('div', { class: 'login-form-card' },
        el('h2', {}, '账号登录'),
        el('div', { class: 'field' }, el('label', {}, '账号'), userInput),
        el('div', { class: 'field' }, el('label', {}, '密码'), passInput),
        errBox,
        loginBtn,
        el('div', { class: 'demo-accounts' },
          el('div', { class: 'label' }, '演示账号（点击填充，密码统一 gt123456）'),
          el('div', { class: 'acct-grid' },
            DEMO_ACCOUNTS.map(a => el('button', {
              class: 'acct-btn',
              onclick: () => { userInput.value = a.u; passInput.value = 'gt123456'; errBox.textContent = ''; },
            },
              el('div', { class: 'r' }, a.role),
              el('div', { class: 'u' }, `${a.u} · ${a.name}`),
            )),
          ),
        ),
      ),
    ),
  );
}
