// ========== 工资分账：版本单价 × 出勤工日；结算冻结、部分发薪不冲销、差额调整 ==========
import { api, cachedGet, getUser } from '../api.js';
import { el, toast, openModal, dataTimestamp, errorState, skeletonCard } from '../ui.js';

function todayMonth() { return new Date().toISOString().slice(0, 7); }
function shiftMonth(month, n) {
  const [y, m] = month.split('-').map(Number);
  const t = new Date(y, m - 1 + n, 1);
  return `${t.getFullYear()}-${String(t.getMonth() + 1).padStart(2, '0')}`;
}
function money(v) { return '¥' + (Number(v) || 0).toFixed(2); }

const LINE_STATUS = {
  preview: ['试算', 'tag'], unpaid: ['待发', 'tag lv-warning'],
  partial: ['部分已发', 'tag lv-warning'], paid: ['已发清', 'tag lv-ok'],
};

export async function renderPayroll(root, params) {
  const user = getUser();
  const isWorker = user.role === 'worker';
  const readOnly = user.role === 'regulator';
  const month = params.get('month') || todayMonth();

  const head = el('div', { class: 'page-head' },
    el('div', {},
      el('h1', {}, '工资分账'),
      el('div', { class: 'sub' }, isWorker
        ? '本人月度工资明细（工日 × 合同日单价）'
        : '按出勤工日 × 合同版本日单价；已结算月份冻结原单价，新版本只影响之后'),
    ),
    el('div', { class: 'actions' },
      el('span', { class: 'range-nav' },
        el('button', { class: 'btn sm ghost', onclick: () => location.hash = '#/payroll?month=' + shiftMonth(month, -1) }, '‹'),
        el('span', { class: 'range-label' }, month),
        el('button', { class: 'btn sm ghost', onclick: () => location.hash = '#/payroll?month=' + shiftMonth(month, 1) }, '›'),
      ),
    ),
  );

  const box = el('div', {}, skeletonCard(420));
  root.innerHTML = '';
  root.append(head, box);

  let res;
  try {
    res = await cachedGet('payroll:' + month + ':' + (isWorker ? 'me' : user.id), '/api/payroll/month?month=' + month);
  } catch (e) {
    box.innerHTML = '';
    if (e.status === 403) box.append(errorState({ icon: '⛔', title: '无权查看分账', desc: e.message, backHash: '#/payroll' }));
    else box.append(errorState({
      icon: '📡', title: '分账数据加载失败',
      desc: e.offline ? '当前离线且没有分账缓存，联网后自动恢复。' : (e.message || '服务暂时不可用。'),
      backHash: '#/payroll',
    }));
    return;
  }
  const d = res.data;
  const frozen = !!d.frozen;
  const canManage = !isWorker && !readOnly;

  // ---- 冻结/试算横幅 ----
  const banner = frozen
    ? el('div', { class: 'freeze-banner' },
        el('span', { class: 'fb-ico' }, '🔒'),
        el('div', { class: 'grow' },
          el('div', { class: 'fb-title' },
            month + ' 已结算冻结',
            el('span', { class: 'tag' }, ({ settled: '待发薪', partial: '部分已发', paid: '已发清' })[d.status] || d.status),
          ),
          el('div', { class: 'fb-desc' },
            `结算于 ${d.settled_at ? d.settled_at.slice(0, 16).replace('T', ' ') : '-'} · 操作人 ${d.settled_by || '-'}。` +
            '本表工日与单价为冻结快照，之后合同调价/出勤修正均不重算、不冲销已发金额。' +
            (d.frozen_note ? ` 说明：${d.frozen_note}` : '')),
        ),
        canManage && d.can_reopen ? el('button', { class: 'btn sm ghost', onclick: () => reopen(month, () => renderPayroll(root, params)) }, '撤销结算(未发薪)') : null,
      )
    : el('div', { class: 'preview-banner' },
        el('span', { class: 'fb-ico' }, '🧮'),
        el('div', { class: 'grow' },
          el('div', { class: 'fb-title' }, month + ' 未结算 · 实时试算'),
          el('div', { class: 'fb-desc' }, d.notice || '按当前合同版本试算，结算后冻结。'),
        ),
        canManage ? el('button', { class: 'btn primary sm', onclick: () => settle(month, () => renderPayroll(root, params)) }, '结算并冻结本月') : null,
      );

  // ---- 汇总 ----
  const summary = el('div', { class: 'stat-row' },
    el('div', { class: 'stat-card c-pending' }, el('div', { class: 'num' }, money(d.total_amount)), el('div', { class: 'label' }, frozen ? '冻结应付总额' : '试算应付总额')),
    el('div', { class: 'stat-card c-onsite' }, el('div', { class: 'num' }, money(d.paid_amount)), el('div', { class: 'label' }, '已发金额(只增不减)')),
    el('div', { class: 'stat-card c-leave' }, el('div', { class: 'num' }, money(d.remaining)), el('div', { class: 'label' }, frozen ? '待发余额' : '待结算')),
    el('div', { class: 'stat-card c-hazard' }, el('div', { class: 'num' }, (d.lines || []).length), el('div', { class: 'label' }, '计薪人数')),
  );

  // ---- 明细表 ----
  const lines = d.lines || [];
  const tableCard = el('div', { class: 'card' },
    el('h3', {}, '分账明细',
      el('span', { class: 'spacer' }),
      el('span', { style: 'display:flex;gap:6px' },
        frozen && canManage && d.status !== 'paid' ? el('button', { class: 'btn sm', onclick: () => payAll(month, () => renderPayroll(root, params)) }, '批量发清') : null,
        frozen && canManage ? el('button', { class: 'btn sm ghost', onclick: () => openContracts(root, params) }, '合同版本') : null,
        !frozen && user.role === 'gc_admin' ? el('button', { class: 'btn sm ghost', onclick: () => openContracts(root, params) }, '合同版本') : null,
      ),
    ),
  );
  if (!lines.length) {
    tableCard.append(el('div', { class: 'empty' },
      frozen ? '该月没有分账明细（可能无在册计薪人员）' : '该月暂无可计薪出勤：出勤/迟到/早退才计工日，缺勤与休息不计。'));
  } else {
    tableCard.append(el('div', { class: 'pay-table-wrap' }, el('table', { class: 'pay-table' },
      el('thead', {}, el('tr', {},
        el('th', {}, '人员'), el('th', {}, '班组'),
        el('th', { class: 'r' }, '计薪工日'), el('th', { class: 'r' }, '日单价/版本'),
        el('th', { class: 'r' }, '冻结金额'), el('th', { class: 'r' }, '差额调整'),
        el('th', { class: 'r' }, '应付'), el('th', { class: 'r' }, '已发'), el('th', { class: 'r' }, '待发'),
        el('th', {}, '状态'), canManage ? el('th', {}, '操作') : null,
      )),
      el('tbody', {}, lines.map(l => el('tr', { class: l.status === 'preview' ? 'preview-row' : '' },
        el('td', {}, el('a', { href: '#/attendance/' + l.worker_id }, l.name + ' ' + l.job_no)),
        el('td', { class: 'muted' }, l.team),
        el('td', { class: 'r' }, Number(l.work_days || 0).toFixed(2) + (l.late_days ? ` (迟${l.late_days})` : '') + (l.early_days ? ` (退${l.early_days})` : '')),
        el('td', { class: 'r' }, money(l.daily_rate)),
        el('td', { class: 'r' }, money(l.amount)),
        el('td', { class: 'r' }, l.adjustment ? money(l.adjustment) : '—'),
        el('td', { class: 'r strong' }, money(l.payable)),
        el('td', { class: 'r' }, money(l.paid_amount)),
        el('td', { class: 'r ' + (l.remaining > 0 ? 'warn' : 'ok') }, money(l.remaining)),
        el('td', {}, (() => { const [lab, cls] = LINE_STATUS[l.status] || [l.status, 'tag']; return el('span', { class: cls }, lab); })()),
        canManage ? el('td', { class: 'pay-ops' },
          frozen && l.remaining > 0 ? el('button', { class: 'btn sm primary', onclick: () => payOne(l, month, () => renderPayroll(root, params)) }, '发薪') : null,
          frozen ? el('button', { class: 'btn sm ghost', onclick: () => adjustOne(l, month, () => renderPayroll(root, params)) }, '差额') : null,
        ) : null,
      ))),
    )));
    // 版本日期以悬浮提示更清晰：重绘第二列
    tableCard.querySelectorAll('tbody tr').forEach((tr, i) => {
      const l = lines[i];
      if (l.rate_version_date) tr.children[3].title = '单价版本生效日 ' + l.rate_version_date;
    });
  }

  box.innerHTML = '';
  box.append(
    el('div', { style: 'display:flex;justify-content:flex-end;margin-bottom:8px' }, dataTimestamp(res.cachedAt, res.stale)),
    banner, summary, tableCard,
  );
}

// ---------- 结算 ----------
function settle(month, reload) {
  const noteI = el('input', { type: 'text', placeholder: '选填：结算说明（写入冻结记录）' });
  const errBox = el('div', { class: 'login-error' });
  openModal({
    title: '结算并冻结 ' + month + ' 月分账？',
    sub: '冻结后：工日与当时合同单价被快照；之后的新单价、出勤修正都不会重算本月，已发金额永不冲销。',
    body: el('div', {},
      el('div', { class: 'warn-box' }, '请先在出勤打卡页核对异常（迟到/早退/缺勤/作废）。结算不可逆（在未发薪前可撤销）。'),
      el('div', { class: 'field' }, el('label', {}, '结算说明'), noteI), errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      {
        label: '确认结算冻结', kind: 'primary',
        onClick: async (close) => {
          try {
            const r = await api.post('/api/payroll/settle', { month, note: noteI.value });
            close(); toast(r.notice, 'ok', 4000); reload();
          } catch (e) { errBox.textContent = e.message; }
        },
      },
    ],
  });
}

async function reopen(month, reload) {
  // 撤销结算仅用于未发薪月：后端目前未提供，直接提示走差额（保守起见不放危险操作）
  toast('已结算月份不支持直接撤销；如需变更请使用「差额调整」', 'warn', 3600);
}

// ---------- 发薪（部分发，不超额，已发不冲减） ----------
function payOne(l, month, reload) {
  const amtI = el('input', { type: 'number', step: '0.01', min: '0', value: String(l.remaining), placeholder: '本次发放金额' });
  const noteI = el('input', { type: 'text', placeholder: '选填：如 现金/银行代发' });
  const errBox = el('div', { class: 'login-error' });
  openModal({
    title: `发薪 · ${l.name} ${l.job_no}`,
    sub: `${month} 月　应付 ${money(l.payable)}　已发 ${money(l.paid_amount)}　待发 ${money(l.remaining)}`,
    body: el('div', {},
      el('div', { class: 'warn-box' }, '可分多次发放：本次只发一部分即「部分已发」；已发金额只增不减，后续重算不会冲销。金额不能超过待发。'),
      el('div', { class: 'field' }, el('label', {}, '本次发放金额（元）'), amtI,
        el('button', { class: 'btn sm ghost', style: 'margin-top:6px', onclick: () => { amtI.value = String(l.remaining); } }, '填满待发 ' + money(l.remaining))),
      el('div', { class: 'field' }, el('label', {}, '备注'), noteI), errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      {
        label: '确认发放', kind: 'primary',
        onClick: async (close) => {
          const amount = Number(amtI.value);
          if (!(amount > 0)) { errBox.textContent = '请输入大于 0 的发放金额'; return; }
          try {
            const r = await api.post('/api/payroll/pay', { month, worker_id: l.worker_id, amount, note: noteI.value });
            close(); toast(r.notice, 'ok', 3800); reload();
          } catch (e) { errBox.textContent = e.message; }
        },
      },
    ],
  });
}

function payAll(month, reload) {
  openModal({
    title: '批量发清 ' + month + ' 月全部待发？',
    sub: '将对所有「待发余额 > 0」的人员按余额足额发放。已部分发放的只补发剩余部分，不会重复发。',
    body: el('div', { class: 'warn-box' }, '发薪流水只增不减；请确认银行代发文件已生成。'),
    actions: [
      { label: '取消', kind: 'ghost' },
      {
        label: '确认批量发清', kind: 'primary',
        onClick: async (close) => {
          try {
            const r = await api.post('/api/payroll/pay', { month, pay_all: true });
            close(); toast(r.notice, 'ok', 3800); reload();
          } catch (e) { toast(e.message, 'err'); }
        },
      },
    ],
  });
}

// ---------- 差额调整（冻结月补差/扣减，单列留痕，不碰原快照与已发） ----------
function adjustOne(l, month, reload) {
  const amtI = el('input', { type: 'number', step: '0.01', placeholder: '正数=补，负数=扣' });
  const reasonI = el('textarea', { rows: '3', placeholder: '如：冻结月出勤作废核减 / 高温补贴补差（必填）' });
  const errBox = el('div', { class: 'login-error' });
  openModal({
    title: `差额调整 · ${l.name} ${l.job_no}`,
    sub: `${month} 冻结金额 ${money(l.amount)}，已发 ${money(l.paid_amount)}。差额单列，不改原快照、不冲销已发。`,
    body: el('div', {},
      el('div', { class: 'warn-box' }, '调整后应付不得低于已发金额（已发不能被冲成负数）；需扣款时请控制金额或线下追回后登记。'),
      el('div', { class: 'field' }, el('label', {}, '差额金额（元，正补负扣）'), amtI),
      el('div', { class: 'field' }, el('label', {}, '原因（必填，留痕）'), reasonI), errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      {
        label: '登记差额', kind: 'primary',
        onClick: async (close) => {
          const amount = Number(amtI.value);
          if (!amount) { errBox.textContent = '请输入非 0 差额'; return; }
          if (!reasonI.value.trim()) { errBox.textContent = '请填写原因'; return; }
          try {
            const r = await api.post('/api/payroll/adjust', { month, worker_id: l.worker_id, amount, reason: reasonI.value });
            close(); toast(r.notice, 'warn', 3800); reload();
          } catch (e) { errBox.textContent = e.message; }
        },
      },
    ],
  });
}

// ---------- 合同版本 ----------
async function openContracts(root, params) {
  const user = getUser();
  const listBox = el('div', {}, skeletonCard(160));
  const close = openModal({
    title: '合同日单价版本',
    sub: '新版本自生效日起仅影响之后的出勤；历史已结算月份冻结在各自版本。',
    body: listBox,
    actions: [{ label: '关闭', kind: 'ghost' }],
  });
  let res;
  try {
    res = await api.get('/api/contracts');
  } catch (e) { listBox.innerHTML = ''; listBox.append(el('div', { class: 'login-error' }, e.message)); return; }
  const groups = {};
  (res.contracts || []).forEach(c => { (groups[c.team] = groups[c.team] || []).push(c); });
  listBox.innerHTML = '';
  Object.entries(groups).forEach(([team, cs]) => {
    listBox.append(el('div', { class: 'contract-team' },
      el('div', { class: 'ct-name' }, team),
      el('div', { class: 'ct-vers' }, cs.map(c =>
        el('div', { class: 'ct-ver' },
          el('span', { class: 'strong' }, money(c.daily_rate) + '/工日'),
          el('span', { class: 'muted' }, '自 ' + c.valid_from + ' 生效'),
          el('span', { class: 'tag' }, '早退折算 ' + c.half_rate_fraction),
          c.note ? el('span', { class: 'muted' }, c.note) : null,
        ))),
    ));
  });
  if (user.role === 'gc_admin') {
    const teams = ['宏宇劳务·钢筋一班', '宏宇劳务·木工二班', '中建劳务·架子班', '安捷租赁·机械班'];
    const teamI = el('select', {}, teams.map(t => el('option', { value: t }, t)));
    const rateI = el('input', { type: 'number', step: '1', placeholder: '新日单价，如 320' });
    const fromI = el('input', { type: 'date', value: new Date().toISOString().slice(0, 10) });
    const noteI = el('input', { type: 'text', placeholder: '调差说明' });
    const errBox = el('div', { class: 'login-error' });
    listBox.append(el('div', { class: 'new-contract card' },
      el('div', { class: 'ct-name' }, '登记新版本单价'),
      el('div', { class: 'field-row' },
        el('div', { class: 'field' }, el('label', {}, '班组'), teamI),
        el('div', { class: 'field' }, el('label', {}, '日单价(元)'), rateI),
        el('div', { class: 'field' }, el('label', {}, '生效日'), fromI)),
      el('div', { class: 'field' }, el('label', {}, '说明'), noteI),
      errBox,
      el('button', {
        class: 'btn primary',
        onclick: async () => {
          try {
            const r = await api.post('/api/contracts', {
              team: teamI.value, daily_rate: Number(rateI.value), half_rate_fraction: 0.5,
              valid_from: fromI.value, note: noteI.value,
            });
            toast(r.notice, 'warn', 4200);
            close(); renderPayroll(root, params);
          } catch (e) { errBox.textContent = e.message; }
        },
      }, '保存新版本（只影响之后）'),
    ));
  }
}
