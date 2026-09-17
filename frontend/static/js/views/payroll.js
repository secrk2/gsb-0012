// ========== 工资分账：月切换 / 单价版本 / 冻结 / 部分发放 / 复核补差 ==========
import { api, cachedGet, getUser } from '../api.js';
import { el, toast, openModal, dataTimestamp, errorState, skeletonCard } from '../ui.js';
import { fmtMoney } from '../calibers.js';

export async function renderPayroll(root, params) {
  const me = getUser();
  const month = params.get('month') || '';
  root.innerHTML = '';
  root.append(skeletonCard(120), el('div', { style: 'height:12px' }), skeletonCard(420));

  let sites = [];
  if (me.role === 'regulator') {
    try {
      const sr = await cachedGet('sites', '/api/sites');
      sites = sr.data.sites || [];
    } catch { sites = []; }
  }
  const siteId = params.get('site_id') || (sites[0] ? String(sites[0].id) : '');

  const qs = new URLSearchParams();
  if (month) qs.set('month', month);
  if (me.role === 'regulator' && siteId) qs.set('site_id', siteId);

  let res;
  try {
    res = await cachedGet('payroll:' + qs.toString(), '/api/payroll' + (qs.toString() ? '?' + qs : ''));
  } catch (e) {
    root.innerHTML = '';
    root.append(errorState({
      icon: '📡', title: '分账数据加载失败',
      desc: e.offline ? '当前离线且本地没有缓存数据。已结算月份为冻结数据，联网后可查看；未结算金额需联网实时计算。' : e.message,
    }));
    return;
  }
  const d = res.data;
  const settled = d.status === 'settled';
  const canManage = !!d.can_manage;

  // ---------- 月切换 ----------
  const months = d.months || [];
  const monthSel = el('select', {
    class: 'search-input',
    onchange: (e) => { location.hash = '#/payroll?month=' + e.target.value; },
  }, months.map(m => el('option', {
    value: m.month,
    selected: m.month === d.month ? '' : null,
  }, m.month + '月' + (m.status === 'settled' ? '（已冻结）' : '') + (m.dirty_count > 0 ? ` · ${m.dirty_count}人待复核` : ''))));

  const siteSel = me.role === 'regulator' && sites.length > 1
    ? el('select', {
        class: 'search-input',
        onchange: (e) => { location.hash = '#/payroll?month=' + d.month + '&site_id=' + e.target.value; },
      }, sites.map(s => el('option', { value: String(s.id), selected: String(s.id) === siteId ? '' : null }, s.name)))
    : null;

  const exportBtn = el('button', { class: 'btn sm ghost', onclick: () => downloadPayrollCsv(d.month, siteId) }, '⬇ 导出分账CSV');

  const head = el('div', { class: 'page-head' },
    el('div', {},
      el('h1', {}, '工资分账'),
      el('div', { class: 'sub' }, '月工资 = 出勤工日（有效打卡工时÷8）× 当月合同日单价；已结算月冻结原单价版本，已发部分永不冲销'),
    ),
    el('div', { class: 'actions' }, siteSel, monthSel, exportBtn, dataTimestamp(res.cachedAt, res.stale)),
  );

  // ---------- 状态横幅 ----------
  const banner = settled
    ? el('div', { class: 'freeze-banner' },
        el('div', { class: 'fb-icon' }, '🔒'),
        el('div', { class: 'grow' },
          el('div', { class: 'fb-title' }, `${d.month} 月已结算冻结`),
          el('div', { class: 'fb-desc' }, `结算时间 ${fmtTimeShort(d.settled_at)} · 结算人 ${d.settled_by_name || '—'}。单价版本、工日、金额均为当时快照；合同新单价只影响之后月份。打卡更正只产生差异，走复核补差。`),
        ),
        d.totals.dirty_count > 0
          ? el('span', { class: 'red-dot' }, d.totals.dirty_count)
          : null)
    : el('div', { class: 'open-banner' },
        el('div', { class: 'fb-icon' }, '🧮'),
        el('div', { class: 'grow' },
          el('div', { class: 'fb-title' }, `${d.month} 月未结算（金额按当前打卡与现行单价实时计算）`),
          el('div', { class: 'fb-desc' }, '结算后将按当时合同版本冻结；之后调价不影响本月。'),
        ),
        canManage && (d.items || []).length
          ? el('button', { class: 'btn primary sm', onclick: () => openSettleModal(d, () => renderPayroll(root, params)) }, '结算本月并冻结')
          : null);

  // ---------- 汇总 ----------
  const t = d.totals || {};
  const summary = el('div', { class: 'stat-row payroll-stats' },
    statCard(fmtMoney(t.frozen_amount), settled ? '冻结金额合计' : '实时金额合计', 'c-pending'),
    statCard(fmtMoney(t.adjustment || 0), '复核补差合计', t.adjustment < 0 ? 'c-alert' : 'c-ok'),
    statCard(fmtMoney(t.final_amount), '应发合计（冻结+补差）', 'c-onsite'),
    statCard(fmtMoney(t.paid_amount), '已发金额（独立留底不可冲销）', 'c-hazard'),
    statCard(fmtMoney(t.pending_amount), '待发金额', 'c-leave'),
    statCard(String(t.people || 0), '计薪人数', 'c-pending'),
  );
  const dirtyWarn = t.dirty_count > 0
    ? el('div', { class: 'dirty-warn' },
        `⚠️ ${t.dirty_count} 人在结算后打卡被改动：冻结金额与已发工资未动，请逐人复核（补差或维持）。`)
    : null;

  // ---------- 明细表（桌面）/ 卡片（手机） ----------
  const items = d.items || [];
  let tableWrap;
  if (!items.length) {
    tableWrap = el('div', { class: 'error-state' },
      el('div', { class: 'icon' }, '📭'),
      el('h2', {}, settled ? '本月结算清单为空' : '本月暂无可计薪工日'),
      el('p', {}, settled ? '该月没有人进入结算快照。' : '所有人当月出勤工日均为0（无有效打卡），待闸机/手机产生打卡后再查看。'));
  } else {
    tableWrap = el('div', {},
      el('div', { class: 'pay-table-desktop' }, buildTable(items, settled, canManage, d.month, () => renderPayroll(root, params))),
      el('div', { class: 'pay-cards' }, ...items.map(it => buildCard(it, settled, canManage, d.month, () => renderPayroll(root, params)))),
    );
  }

  const calNote = el('div', { class: 'caliber-note' },
    '📐 工日与出勤率口径：工日 = 有效打卡工时 ÷ 8（半天班出勤半天=0.5工日）；出勤率双口径（实际出勤天/应出勤天、有效打卡工时/排班工时）在作战台、出勤甘特、CSV 导出中一致。');

  root.innerHTML = '';
  root.append(head, banner, summary, dirtyWarn, tableWrap, calNote);
}

function statCard(num, label, cls) {
  return el('div', { class: 'stat-card ' + cls }, el('div', { class: 'num money-num' }, num), el('div', { class: 'label' }, label));
}

function fmtTimeShort(iso) {
  if (!iso) return '—';
  const x = new Date(iso);
  if (isNaN(x)) return iso;
  const p = n => String(n).padStart(2, '0');
  return `${x.getFullYear()}-${p(x.getMonth() + 1)}-${p(x.getDate())}`;
}

function buildTable(items, settled, canManage, month, reload) {
  const th = (txt) => el('th', {}, txt);
  const head = el('tr', {},
    th('人员'), th('班组/工种'), th('单价版本'), th('日单价'), th('工日'), th('有效工时'),
    th(settled ? '冻结金额' : '实时金额'), th('补差'), th('应发'), th('已发'), th('待发'), th('操作'));
  const rows = items.map(it => {
    const actionBtns = el('div', { class: 'row-actions' },
      el('button', { class: 'btn sm ghost', onclick: () => openContractModal(it, reload) }, '合同版本'),
      canManage && settled ? el('button', { class: 'btn sm ghost', onclick: () => openPayModal([it], month, reload) }, '登记发放') : null,
      canManage && settled && it.dirty ? el('button', { class: 'btn sm danger', onclick: () => openReviewModal(it, month, reload) }, '复核差异') : null,
    );
    return el('tr', { class: it.dirty ? 'row-dirty' : '' },
      el('td', {}, el('div', { class: 'at-pname' }, it.name, it.dirty ? el('span', { class: 'mini-badge danger' }, '待复核') : null), el('div', { class: 'at-pmeta' }, it.job_no)),
      el('td', {}, el('div', {}, it.team), el('div', { class: 'dim' }, it.trade)),
      el('td', {}, 'v' + it.rate_version),
      el('td', {}, fmtMoney(it.day_rate)),
      el('td', {}, (it.work_days || 0).toFixed(2)),
      el('td', {}, (it.work_hours || 0).toFixed(0) + 'h'),
      el('td', {}, fmtMoney(it.amount)),
      el('td', { class: it.adjustment < 0 ? 'neg' : (it.adjustment > 0 ? 'pos' : '') }, it.adjustment ? fmtMoney(it.adjustment) : '—'),
      el('td', { class: 'strong' }, fmtMoney(it.final_amount)),
      el('td', {}, fmtMoney(it.paid_amount), it.overpaid ? el('div', { class: 'neg small' }, '已超发') : null),
      el('td', { class: 'strong' }, it.pending_amount > 0 ? fmtMoney(it.pending_amount) : '已发清'),
      el('td', {}, actionBtns),
    );
  });
  return el('table', { class: 'pay-table' }, el('thead', {}, head), el('tbody', {}, rows));
}

function buildCard(it, settled, canManage, month, reload) {
  return el('div', { class: 'card pay-card' + (it.dirty ? ' dirty' : '') },
    el('div', { class: 'pc-head' },
      el('div', {},
        el('div', { class: 'at-pname' }, it.name, it.dirty ? el('span', { class: 'mini-badge danger' }, '待复核') : null),
        el('div', { class: 'at-pmeta' }, `${it.job_no} · ${it.team} · ${it.trade}`),
      ),
      el('div', {}, el('span', { class: 'tag' }, 'v' + it.rate_version), ' ', el('span', { class: 'tag' }, fmtMoney(it.day_rate) + '/工日')),
    ),
    el('div', { class: 'pc-grid' },
      kv('工日', (it.work_days || 0).toFixed(2)),
      kv('有效工时', (it.work_hours || 0).toFixed(0) + 'h'),
      kv(settled ? '冻结金额' : '实时金额', fmtMoney(it.amount)),
      kv('补差', it.adjustment ? fmtMoney(it.adjustment) : '—'),
      kv('应发', fmtMoney(it.final_amount)),
      kv('已发', fmtMoney(it.paid_amount)),
      kv('待发', it.pending_amount > 0 ? fmtMoney(it.pending_amount) : '已发清'),
    ),
    el('div', { class: 'pc-actions' },
      el('button', { class: 'btn sm ghost', onclick: () => openContractModal(it, reload) }, '合同版本'),
      canManage && settled ? el('button', { class: 'btn sm ghost', onclick: () => openPayModal([it], month, reload) }, '登记发放') : null,
      canManage && settled && it.dirty ? el('button', { class: 'btn sm danger', onclick: () => openReviewModal(it, month, reload) }, '复核差异') : null,
    ),
  );
}

function kv(k, v) {
  return el('div', { class: 'pc-kv' }, el('div', { class: 'k' }, k), el('div', { class: 'v' }, v));
}

// ---------- 结算（二次确认冻结） ----------
function openSettleModal(d, reload) {
  const errBox = el('div', { class: 'login-error' });
  openModal({
    title: `结算 ${d.month} 月工资`,
    sub: '结算即冻结：单价版本、工日、金额写入快照，之后合同调价不影响本月',
    body: el('div', {},
      el('div', { class: 'warn-box' },
        `本次将冻结 ${d.totals.people} 人，合计 ${fmtMoney(d.totals.frozen_amount)}（按当前合同版本与当前打卡工日）。历史已结算月份不受影响。`),
      el('div', { class: 'settle-list' }, (d.items || []).slice(0, 8).map(it =>
        el('div', { class: 'settle-row' }, el('span', {}, `${it.name} ${it.job_no}`), el('span', { class: 'dim' }, `${(it.work_days || 0).toFixed(2)}工日 × v${it.rate_version} ${fmtMoney(it.day_rate)}`), el('strong', {}, fmtMoney(it.amount))))),
      (d.items || []).length > 8 ? el('div', { class: 'dim' }, `其余 ${d.items.length - 8} 人略，导出 CSV 可看全量`) : null,
      errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      {
        label: '确认结算并冻结', kind: 'primary',
        onClick: async (close) => {
          errBox.textContent = '';
          try {
            const r = await api.post('/api/payroll/settle', { month: d.month, confirm: true });
            close(); toast(r.message, 'ok', 4200); reload();
          } catch (e) { errBox.textContent = e.message; }
        },
      },
    ],
  });
}

// ---------- 登记发放（支持部分发放，已发部分不被重算） ----------
function openPayModal(items, month, reload) {
  const rows = items.map(it => {
    const chk = el('input', { type: 'checkbox', checked: it.pending_amount > 0 ? '' : null, disabled: it.pending_amount > 0 ? null : '' });
    const amt = el('input', { type: 'number', class: 'pay-amt', value: String(it.pending_amount || 0), step: '0.01', min: '0' });
    if (!(it.pending_amount > 0)) { chk.checked = false; }
    return { it, chk, amt };
  });
  const noteInput = el('input', { type: 'text', placeholder: '如：8月工资首批，银行代发批次20260905' });
  const errBox = el('div', { class: 'login-error' });
  openModal({
    title: `登记发放 · ${month} 月`,
    sub: '已发金额独立留底；后续任何重算/复核补差都不会冲销已发部分，可多次部分发放',
    body: el('div', {},
      el('div', { class: 'pay-modal-list' }, rows.map(r => el('label', { class: 'pay-modal-row' },
        r.chk,
        el('span', { class: 'grow' }, r.it.name, ' ', el('span', { class: 'dim' }, r.it.job_no),
          el('div', { class: 'dim small' }, `应发 ${fmtMoney(r.it.final_amount)} · 已发 ${fmtMoney(r.it.paid_amount)} · 待发 ${fmtMoney(r.it.pending_amount)}`)),
        el('span', {}, '¥'), r.amt))),
      el('div', { class: 'field' }, el('label', {}, '发放备注'), noteInput),
      errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      {
        label: '确认登记发放', kind: 'primary',
        onClick: async (close) => {
          const workers = rows.filter(r => r.chk.checked).map(r => ({ worker_id: r.it.worker_id, amount: Number(r.amt.value || 0) }));
          if (!workers.length) { errBox.textContent = '请勾选至少一人'; return; }
          if (workers.some(w => w.amount <= 0)) { errBox.textContent = '发放金额必须大于0'; return; }
          try {
            const r = await api.post('/api/payroll/pay', { month, workers, note: noteInput.value.trim() });
            close(); toast(r.message, 'ok', 4200); reload();
          } catch (e) { errBox.textContent = e.message; }
        },
      },
    ],
  });
}

// ---------- 复核差异（二次确认+原因留痕） ----------
function openReviewModal(it, month, reload) {
  const diff = it.current_amount - it.amount;
  const signed = (n) => (n >= 0 ? '+¥' : '-¥') + Math.abs(n).toFixed(2);
  const reason = el('textarea', { rows: '3', placeholder: '必填：如「刷脸复核确认08-25代打卡，按作废后实际工日补差」' });
  const errBox = el('div', { class: 'login-error' });
  const submit = async (action) => {
    if (reason.value.trim().length < 2) { errBox.textContent = '请填写复核原因（不少于2个字），将留痕'; return; }
    try {
      const r = await api.post('/api/payroll/review', { month, worker_id: it.worker_id, action, reason: reason.value.trim() });
      close2(); toast(r.message, 'warn', 4600); reload();
    } catch (e) { errBox.textContent = e.message; }
  };
  let close2;
  close2 = openModal({
    title: `复核打卡差异 · ${it.name}（${it.job_no}）`,
    sub: `${month} 月已冻结，已发 ${fmtMoney(it.paid_amount)} 不冲销；仅在冻结金额基础上补差`,
    body: el('div', {},
      el('div', { class: 'review-grid' },
        kv('冻结金额（原单价快照）', fmtMoney(it.amount)),
        kv('按最新打卡实时金额', fmtMoney(it.current_amount)),
        kv('本次补差', signed(diff)),
        kv('已发金额（不动）', fmtMoney(it.paid_amount)),
        kv('补差后应发（=实时金额）', fmtMoney(it.current_amount)),
        kv('补差后待发', fmtMoney(Math.max(0, it.current_amount - it.paid_amount))),
      ),
      el('div', { class: 'warn-box' }, '选择「按最新口径补差」将生成一条带原因的补差记录；选择「维持原金额」则销掉差异标记，工资不变。补差后应发以最新口径金额为准，已发部分不动。'),
      el('div', { class: 'field' }, el('label', {}, '复核原因（必填，留痕）'), reason),
      errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      { label: '维持原金额', onClick: () => submit('keep') },
      { label: '按最新口径补差', kind: 'danger', onClick: () => submit('apply') },
    ],
  });
}

// ---------- 合同版本 ----------
function openContractModal(it, reload) {
  const listBox = el('div', { class: 'contract-list' }, el('div', { class: 'skeleton', style: 'height:60px' }));
  const errBox = el('div', { class: 'login-error' });
  let close2;
  close2 = openModal({
    title: `合同日单价版本 · ${it.name}（${it.job_no}）`,
    sub: '新版本从选定月份起生效，只影响该月及以后；已结算月份永远冻结在原版本',
    body: el('div', {}, listBox, errBox, el('div', { id: 'new-contract-slot' })),
    actions: [{ label: '关闭', kind: 'ghost' }],
  });
  (async () => {
    try {
      const r = await api.get('/api/contracts?worker_id=' + it.worker_id);
      listBox.innerHTML = '';
      (r.contracts || []).forEach((c, i) => {
        listBox.append(el('div', { class: 'contract-row' + (i === 0 ? ' current' : '') },
          el('div', {}, el('strong', {}, 'v' + c.version + ' ' + fmtMoney(c.day_rate) + '/工日'),
            el('div', { class: 'dim small' }, `${c.effective_from} 起生效 · ${c.note || '—'}`)),
          i === 0 ? el('span', { class: 'tag' }, '现行') : el('span', { class: 'tag' }, '历史冻结'),
        ));
      });
      if (!r.contracts || !r.contracts.length) listBox.append(el('div', { class: 'empty' }, '尚无合同版本'));
      document.getElementById('new-contract-slot').append(newContractForm(it, listBox, errBox, close2, reload));
    } catch (e) {
      listBox.innerHTML = '';
      listBox.append(el('div', { class: 'login-error' }, e.message));
    }
  })();
}

function newContractForm(it, listBox, errBox, close, reload) {
  const rate = el('input', { type: 'number', step: '0.01', min: '0.01', placeholder: '如新单价 350' });
  const month = el('input', { type: 'month', value: new Date().toISOString().slice(0, 7) });
  const note = el('input', { type: 'text', placeholder: '调价说明（如：高温补贴/技能津贴）' });
  const me = getUser();
  if (me.role !== 'gc_admin') {
    return el('div', { class: 'dim small' }, '仅总包管理员可建立新单价版本。');
  }
  return el('div', { class: 'new-contract' },
    el('div', { class: 'nc-title' }, '建立新版本（不追溯已结算月）'),
    el('div', { class: 'nc-row' },
      el('label', {}, '日单价 ¥', rate),
      el('label', {}, '生效月份 ', month)),
    el('div', { class: 'field' }, note),
    el('button', {
      class: 'btn primary sm',
      onclick: async () => {
        errBox.textContent = '';
        if (Number(rate.value) <= 0) { errBox.textContent = '请输入大于0的日单价'; return; }
        try {
          const r = await api.post('/api/contracts', { worker_id: it.worker_id, day_rate: Number(rate.value), effective_from: month.value, note: note.value.trim() });
          toast(r.message, 'ok', 4000); close(); reload();
        } catch (e) { errBox.textContent = e.message; }
      },
    }, '建立新版本'),
  );
}

async function downloadPayrollCsv(month, siteId) {
  try {
    const token = localStorage.getItem('gt_token');
    let url = '/api/payroll/export?month=' + encodeURIComponent(month);
    if (siteId) url += '&site_id=' + encodeURIComponent(siteId);
    const resp = await fetch(url, { headers: { Authorization: 'Bearer ' + token } });
    if (!resp.ok) throw new Error(await resp.text());
    const blob = await resp.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'payroll_' + month + '.csv';
    a.click();
    URL.revokeObjectURL(a.href);
    toast('分账 CSV 已导出（含冻结/补差/已发/待发列）');
  } catch (e) {
    toast('导出失败：' + e.message, 'err', 3600);
  }
}
