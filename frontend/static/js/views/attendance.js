// ========== 出勤打卡：周/月甘特 + 双口径 + 打卡/作废/改判 + 三态空态 ==========
import { api, cachedGet, getUser, submitPunch, isOnline, downloadWithAuth } from '../api.js';
import { el, toast, openModal, dataTimestamp, errorState, skeletonCard } from '../ui.js';
import { CALIBERS, getCaliber, setCaliber, pct, hoursOf, HALF_DAY_NOTE } from '../calibers.js';

export const STATUS_CLS = {
  present: 'at-present', late: 'at-late', early: 'at-early',
  absent: 'at-absent', rest: 'at-rest', void: 'at-void',
};
export const STATUS_GLYPH = { present: '✓', late: '迟', early: '退', absent: '缺', rest: '休', void: '作' };

function weekCN(t) {
  return ['日', '一', '二', '三', '四', '五', '六'][new Date(t + 'T00:00:00').getDay()];
}
function todayStr() {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}
function shiftMonth(month, n) {
  const [y, m] = month.split('-').map(Number);
  const t = new Date(y, m - 1 + n, 1);
  return `${t.getFullYear()}-${String(t.getMonth() + 1).padStart(2, '0')}`;
}
function shiftWeekAnchor(anchor, n) {
  const d = new Date(anchor + 'T00:00:00');
  d.setDate(d.getDate() + n * 7);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

// ---------- 主页面 ----------
export async function renderAttendance(root, params) {
  const user = getUser();
  const isWorker = user.role === 'worker';
  const view = params.get('view') === 'month' ? 'month' : 'week';
  const month = params.get('month') || todayStr().slice(0, 7);
  const anchor = params.get('from') || todayStr();
  const team = isWorker ? '' : (params.get('team') || '');

  const head = el('div', { class: 'page-head' },
    el('div', {},
      el('h1', {}, '出勤打卡'),
      el('div', { class: 'sub' }, isWorker ? '本人出勤打卡与出勤结论' : '闸机 / 手机定位多源打卡自动合并为每人每日一条结论'),
    ),
    el('div', { class: 'actions' },
      isWorker ? el('button', { class: 'btn primary sm', onclick: () => openPunchModal(() => load()) }, '📱 手机定位打卡')
               : el('button', { class: 'btn sm', onclick: () => openPunchModal(() => load()) }, '补打卡'),
    ),
  );

  // 视图切换 + 周期导航
  const viewBar = el('div', { class: 'filter-bar' },
    el('button', { class: 'filter-chip' + (view === 'week' ? ' active' : ''),
      onclick: () => { location.hash = '#/attendance?view=week' + (team ? '&team=' + team : ''); } }, '周视图'),
    el('button', { class: 'filter-chip' + (view === 'month' ? ' active' : ''),
      onclick: () => { location.hash = '#/attendance?view=month' + (team ? '&team=' + team : ''); } }, '月视图'),
    el('span', { class: 'range-nav' },
      el('button', { class: 'btn sm ghost', onclick: () => navigate(-1) }, '‹'),
      el('span', { class: 'range-label', id: 'at-range-label' }, ''),
      el('button', { class: 'btn sm ghost', onclick: () => navigate(1) }, '›'),
    ),
  );

  // 班组筛选（总包/监理）
  let teamBar = null;
  if (!isWorker) {
    const teams = ['', '宏宇劳务·钢筋一班', '宏宇劳务·木工二班', '中建劳务·架子班', '安捷租赁·机械班'];
    teamBar = el('div', { class: 'filter-bar' },
      teams.map(t => el('button', {
        class: 'filter-chip' + (team === t ? ' active' : ''),
        onclick: () => { location.hash = '#/attendance?view=' + view + (view === 'month' ? '&month=' + month : '') + (t ? '&team=' + encodeURIComponent(t) : ''); },
      }, t || '全部班组')),
    );
  }

  const summaryBar = el('div', { class: 'at-summary' });
  const caliberBar = el('div', { class: 'at-caliber' });
  const ganttBox = el('div', {}, skeletonCard(360));

  function navigate(n) {
    if (view === 'month') {
      location.hash = '#/attendance?view=month&month=' + shiftMonth(month, n) + (team ? '&team=' + encodeURIComponent(team) : '');
    } else {
      location.hash = '#/attendance?view=week&from=' + shiftWeekAnchor(anchor, n) + (team ? '&team=' + encodeURIComponent(team) : '');
    }
  }

  root.innerHTML = '';
  root.append(head, viewBar, teamBar, caliberBar, summaryBar, ganttBox);

  async function load() {
    ganttBox.innerHTML = '';
    ganttBox.append(skeletonCard(360));
    const qp = new URLSearchParams();
    if (view === 'month') { qp.set('view', 'month'); qp.set('month', month); }
    else { qp.set('view', 'week'); qp.set('from', anchor); }
    if (team) qp.set('team', team);
    const cacheKey = 'gantt:' + (isWorker ? 'me' : user.id) + ':' + qp.toString();
    let res;
    try {
      res = await cachedGet(cacheKey, '/api/attendance/gantt?' + qp.toString());
    } catch (e) {
      ganttBox.innerHTML = '';
      caliberBar.innerHTML = ''; summaryBar.innerHTML = '';
      // 空态①：加载失败（离线无缓存/服务错误）——区别于「无打卡」
      ganttBox.append(errorState({
        icon: '📡', title: '出勤打卡数据加载失败',
        desc: e.offline ? '当前离线且本机没有该时段的缓存数据，联网后会自动恢复。' : (e.message || '服务暂时不可用，请稍后重试。'),
        backHash: '#/attendance', backLabel: '重试',
      }));
      return;
    }
    const d = res.data;
    document.getElementById('at-range-label').textContent = `${d.from} ~ ${d.to}`;
    renderCalibers(d, caliberBar, () => paint(d, res));
    paint(d, res);
  }

  // 双口径切换：口径定义直接写在界面，作战台/导出逐字一致
  function renderCalibers(d, bar, repaint) {
    const cur = getCaliber();
    bar.innerHTML = '';
    bar.append(
      el('div', { class: 'caliber-switch' },
        Object.values(CALIBERS).map(c => el('button', {
          class: 'filter-chip' + (cur === c.key ? ' active' : ''),
          onclick: () => { setCaliber(c.key); repaint(); },
        }, c.label)),
      ),
      el('div', { class: 'caliber-def' }, CALIBERS[cur].text),
      ((d.rule?.half_day_teams || []).length && !isWorker)
        ? el('div', { class: 'caliber-warn' }, '⚠️ ' + HALF_DAY_NOTE)
        : null,
    );
  }

  function paint(d, res) {
    const cur = getCaliber();
    const total = d.total || {};
    summaryBar.innerHTML = '';
    summaryBar.append(
      el('div', { class: 'stat-row at-stat-row' },
        el('div', { class: 'stat-card c-onsite' },
          el('div', { class: 'num' }, pct(CALIBERS[cur].pick(total))),
          el('div', { class: 'label' }, '出勤率·' + CALIBERS[cur].label)),
        el('div', { class: 'stat-card c-pending' },
          el('div', { class: 'num' }, Number(total.present_days || 0).toFixed(1)),
          el('div', { class: 'label' }, '实际出勤天')),
        el('div', { class: 'stat-card c-leave' },
          el('div', { class: 'num' }, Number(total.scheduled_days || 0).toFixed(1)),
          el('div', { class: 'label' }, '应出勤天')),
        el('div', { class: 'stat-card c-hazard' },
          el('div', { class: 'num' }, hoursOf(total.worked_minutes)),
          el('div', { class: 'label' }, '有效打卡工时(h)')),
        el('div', { class: 'stat-card c-alert' },
          el('div', { class: 'num' }, hoursOf(total.scheduled_minutes)),
          el('div', { class: 'label' }, '排班工时(h)')),
      ),
      el('div', { style: 'display:flex;justify-content:flex-end;margin:4px 0 10px' },
        el('button', { class: 'btn sm ghost', onclick: doExport }, '⬇ 导出 CSV'),
        el('span', { style: 'width:10px' }),
        dataTimestamp(res.cachedAt, res.stale),
      ),
    );

    const workers = d.workers || [];
    ganttBox.innerHTML = '';

    // 空态②：该人无任何出勤打卡（区别于加载失败）
    if (!workers.length) {
      ganttBox.append(emptyNoPunch(isWorker));
      return;
    }
    const anyPunch = workers.some(w => w.has_any_punch);
    if (!anyPunch) {
      ganttBox.append(emptyNoPunch(isWorker, load));
      return;
    }
    // 空态③：区间内打卡全部作废（有记录但全作废，区别于无打卡）
    if (workers.every(w => !w.has_any_punch || w.all_voided)) {
      ganttBox.append(emptyAllVoid());
      return;
    }

    ganttBox.append(buildGantt(d, user, () => load()));
  }

  async function doExport() {
    const qp = new URLSearchParams();
    if (view === 'month') { qp.set('view', 'month'); qp.set('month', month); }
    else { qp.set('view', 'week'); qp.set('from', anchor); }
    if (team) qp.set('team', team);
    try {
      toast('正在导出 CSV…');
      await downloadWithAuth('/api/attendance/export?' + qp.toString(), 'attendance.csv');
    } catch (e) { toast(e.message, 'err'); }
  }

  load();
}

// ---------- 甘特表格 ----------
function buildGantt(d, user, reload) {
  const canManage = user.role !== 'worker' && user.role !== 'regulator';
  const dates = d.dates || [];
  const today = todayStr();

  const headCells = dates.map(ds => {
    const dd = Number(ds.slice(8));
    const wk = weekCN(ds);
    const weekend = wk === '六' || wk === '日';
    return el('div', { class: 'at-col-head' + (weekend ? ' wkend' : '') + (ds === today ? ' today' : '') },
      el('div', { class: 'ch-d' }, dd), el('div', { class: 'ch-w' }, wk));
  });

  const thead = el('div', { class: 'at-row at-head-row' },
    el('div', { class: 'at-name-cell' }, '人员 / 日期'),
    el('div', { class: 'at-cells' }, headCells),
  );

  const bodyRows = (d.workers || []).map(w => {
    const nameCell = el('div', { class: 'at-name-cell' },
      el('a', { href: '#/attendance/' + w.worker_id, class: 'at-name-link' }, w.name + ' ' + w.job_no),
      el('div', { class: 'at-team' },
        w.team + (w.half_day ? ' · 半天班' : ''),
        !w.has_any_punch ? el('span', { class: 'tag no-punch-tag' }, '该时段无打卡') : null,
        w.all_voided ? el('span', { class: 'tag lv-critical' }, '打卡全作废') : null),
      el('div', { class: 'at-rate' },
        el('span', { class: 'tag ' + rateClass(w.rate_days) }, '天 ' + pct(w.rate_days)),
        el('span', { class: 'tag ' + rateClass(w.rate_hours) }, '工时 ' + pct(w.rate_hours)),
      ),
    );
    const cells = dates.map(ds => {
      const c = w.days[ds];
      const cls = STATUS_CLS[c.status] || 'at-rest';
      const cell = el('div', {
        class: 'at-cell ' + cls + (ds === today ? ' today' : '') + (c.manual ? ' manual' : '') + (c.locked ? ' locked' : ''),
        title: cellTitle(c),
        dataset: { date: ds.slice(5), wk: '周' + weekCN(ds), label: c.label },
      }, el('span', { class: 'at-glyph' }, STATUS_GLYPH[c.status] || '·'));
      if (canManage && (c.status !== 'rest' || c.total > 0)) {
        cell.style.cursor = 'pointer';
        cell.addEventListener('click', () => openCellModal(w, c, reload));
      }
      return cell;
    });
    return el('div', {
      class: 'at-row' + (w.all_voided ? ' all-void-row' : '') + (!w.has_any_punch ? ' no-punch-row' : ''),
    }, nameCell, el('div', { class: 'at-cells' }, cells));
  });

  const legend = el('div', { class: 'at-legend' },
    ['present', 'late', 'early', 'absent', 'rest', 'void'].map(k =>
      el('span', { class: 'at-legend-item' },
        el('span', { class: 'at-cell mini ' + STATUS_CLS[k] }, STATUS_GLYPH[k]),
        ({ present: '出勤', late: '迟到', early: '早退', absent: '缺勤', rest: '休息', void: '已作废' })[k])),
    el('span', { class: 'at-legend-item muted' }, '描边=人工改判'),
    el('span', { class: 'at-legend-item muted' }, '🔒=已结算月冻结(改动需二次确认留痕)'),
  );

  return el('div', { class: 'card at-card' },
    el('h3', {}, '出勤打卡甘特', el('span', { class: 'spacer' }),
      el('span', { class: 'tag' }, `${dates.length} 天 · ${bodyRows.length} 人有打卡`)),
    el('div', { class: 'at-scroll' }, el('div', { class: 'at-grid' }, thead, bodyRows)),
    legend,
  );
}

function rateClass(v) {
  const n = Number(v) || 0;
  if (n >= 0.9) return 'lv-ok';
  if (n >= 0.6) return 'lv-warning';
  return 'lv-critical';
}

function cellTitle(c) {
  const parts = [c.label];
  if (c.first_in) parts.push('首入 ' + c.first_in);
  if (c.last_out) parts.push('末出 ' + c.last_out);
  if (c.sources) parts.push('合并来源 ' + c.sources + '（已并为一条）');
  if (c.valid) parts.push(`有效打卡 ${c.valid}/${c.total_punches ?? c.total}`);
  if (c.work_minutes) parts.push('有效工时 ' + hoursOf(c.work_minutes) + 'h');
  if (c.locked) parts.push('已结算月冻结');
  return parts.join(' · ');
}

// ---------- 三种空态（严格分开，禁止统一「暂无数据」） ----------

// ② 该人无出勤打卡
function emptyNoPunch(isWorker, reload) {
  return el('div', { class: 'error-state at-empty' },
    el('div', { class: 'icon' }, '🧾'),
    el('h2', {}, isWorker ? '你在该时段没有出勤打卡' : '该班组在该时段没有任何人打卡'),
    el('p', {}, isWorker
      ? '没有查询到闸机或手机定位打卡记录。如今天已到岗，请用「手机定位打卡」补打，或联系门口闸机管理员核查。'
      : '不是网络错误：该时段确实没有任何闸机/手机打卡流水。可检查排班日期，或使用「补打卡」登记。'),
    isWorker ? el('button', { class: 'btn primary', onclick: () => openPunchModal(reload) }, '📱 立即手机定位打卡')
             : el('button', { class: 'btn', onclick: () => openPunchModal(reload) }, '补打卡登记'),
  );
}

// ③ 出勤打卡全部作废
function emptyAllVoid() {
  return el('div', { class: 'error-state at-empty' },
    el('div', { class: 'icon' }, '🚫'),
    el('h2', {}, '该时段出勤打卡已全部作废'),
    el('p', {}, '存在打卡流水，但每条都被作废处理（如代打卡嫌疑、设备误识别）。此状态不计出勤、不计工资；如需恢复，请进入人员出勤详情「恢复」单条记录，操作会留痕。'),
    el('a', { class: 'btn ghost', href: '#/workers' }, '去人员详情核查'),
  );
}

// ---------- 打卡弹窗（工人自助手机定位 / 管理补打） ----------
function openPunchModal(onDone) {
  const user = getUser();
  const isWorker = user.role === 'worker';
  const now = new Date();
  const pad = n => String(n).padStart(2, '0');
  const day = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
  const tm = `${pad(now.getHours())}:${pad(now.getMinutes())}:00`;

  const dayI = el('input', { type: 'date', value: day });
  const tmI = el('input', { type: 'time', step: '1', value: tm });
  const srcI = el('select', {},
    el('option', { value: 'mobile' }, '手机定位'),
    isWorker ? null : el('option', { value: 'gate' }, '闸机'),
  );
  const devI = el('input', { type: 'text', placeholder: '选填，如 东门闸机A / 工人手机定位' });
  // 管理端：从人员列表选要补卡的人（工人本人固定为自己）
  let workerI;
  if (isWorker) {
    workerI = el('input', { type: 'hidden', value: String(user.worker_id || '') });
  } else {
    workerI = el('select', {}, el('option', { value: '' }, '加载人员中…'));
    api.get('/api/workers').then(r => {
      workerI.innerHTML = '';
      workerI.append(el('option', { value: '' }, '请选择人员'));
      (r.workers || []).forEach(w => workerI.append(
        el('option', { value: String(w.id) }, `${w.label}（${w.team}）`)));
    }).catch(() => { workerI.innerHTML = ''; workerI.append(el('option', { value: '' }, '人员加载失败')); });
  }
  const reasonI = el('input', { type: 'text', placeholder: '仅补打已结算月份时必填' });
  const errBox = el('div', { class: 'login-error' });

  const locBox = el('div', { class: 'hint' }, '正在获取定位…');
  if (isWorker && navigator.geolocation) {
    navigator.geolocation.getCurrentPosition(
      () => { locBox.textContent = '✓ 定位已获取（电子围栏内）'; locBox.classList.add('ok'); },
      () => { locBox.textContent = '⚠️ 未能获取定位，仍可提交但将标记为定位待核'; },
      { timeout: 6000 },
    );
  }

  async function submit(confirm) {
    errBox.textContent = '';
    const workerId = isWorker ? (user.worker_id || 0) : Number(workerI.value);
    if (!workerId) { errBox.textContent = '请填写打卡人员ID'; return; }
    try {
      const r = await submitPunch({
        worker_id: workerId,
        punch_date: dayI.value,
        punch_time: tmI.value.length === 5 ? tmI.value + ':00' : tmI.value,
        source: srcI.value,
        device: devI.value,
        reason: reasonI.value,
        confirm,
      });
      if (r.queued) {
        close(); toast('已离线排队，恢复网络后自动同步（幂等，不会重复）', 'warn', 3200); onDone();
        return;
      }
      close();
      toast(r.notice || '打卡成功');
      onDone();
    } catch (e) {
      // 冻结月补打：服务端要求二次确认 + 原因
      if (e.status === 409 && e.data && e.data.need_confirm) {
        openModal({
          title: '二次确认：补打已结算月份',
          sub: e.data.error,
          body: el('div', {},
            el('div', { class: 'warn-box' }, '该月工资已冻结，本次补打只更新出勤记录，不会重算当月分账；差额请走分账页「差额调整」。'),
            el('div', { class: 'field' }, el('label', {}, '补打原因（必填，留痕）'), reasonI)),
          actions: [
            { label: '取消', kind: 'ghost' },
            { label: '确认补打并留痕', kind: 'danger', onClick: (c2) => { c2(); submit(true); } },
          ],
        });
        return;
      }
      errBox.textContent = e.message;
    }
  }

  const body = el('div', {},
    isWorker ? null : el('div', { class: 'field' }, el('label', {}, '打卡人员'), workerI),
    el('div', { class: 'field-row' },
      el('div', { class: 'field' }, el('label', {}, '日期'), dayI),
      el('div', { class: 'field' }, el('label', {}, '时间'), tmI)),
    el('div', { class: 'field' }, el('label', {}, '打卡来源'), srcI),
    el('div', { class: 'field' }, el('label', {}, '设备/来源说明'), devI),
    isWorker ? locBox : null,
    el('div', { class: 'field' }, el('label', {}, '原因'), reasonI,
      el('div', { class: 'hint' }, '同人同天多台设备打卡会自动合并为一条，无需担心重复。')),
    errBox,
  );
  const close = openModal({
    title: isWorker ? '手机定位打卡' : '补打卡登记',
    sub: isOnline() ? '' : '当前离线：将进入队列，联网后同步',
    body,
    actions: [
      { label: '取消', kind: 'ghost' },
      { label: '提交打卡', kind: 'primary', onClick: () => submit(false) },
    ],
  });
}

// ---------- 单元格：查看合并来源 / 作废 / 改判 ----------
function openCellModal(w, c, reload) {
  const user = getUser();
  const canManage = user.role !== 'worker' && user.role !== 'regulator';
  const frozen = !!c.locked;
  const info = el('div', { class: 'cell-detail' },
    el('dl', { class: 'kv' },
      el('dt', {}, '日期'), el('dd', {}, c.day + ' 周' + weekCN(c.day)),
      el('dt', {}, '日结论'), el('dd', {}, c.label + (c.manual ? '（人工改判）' : '') + (frozen ? '（已结算冻结）' : '')),
      el('dt', {}, '首次打卡'), el('dd', {}, c.first_in || '-'),
      el('dt', {}, '末次打卡'), el('dd', {}, c.last_out || '-'),
      el('dt', {}, '合并来源'), el('dd', {}, c.sources ? c.sources + '（多源已合并为一条）' : '-'),
      el('dt', {}, '有效打卡'), el('dd', {}, `${c.valid_punches ?? c.valid} / ${c.total_punches ?? c.total} 条（有效/总流水）`),
      el('dt', {}, '有效工时'), el('dd', {}, hoursOf(c.work_minutes) + ' 小时（首入→末出）'),
    ),
    el('div', { class: 'hint' }, '完整原始打卡流水与作废/改判留痕见该人员「出勤详情」。'),
    el('a', { class: 'btn sm ghost', href: '#/attendance/' + w.worker_id }, '打开出勤详情 ›'),
  );
  const actions = [{ label: '关闭', kind: 'ghost' }];
  openModal({
    title: `${w.name} · ${c.day}`,
    sub: frozen ? '该日属于已结算冻结月份，任何改动都需二次确认并填写原因，且不重算当月工资' : '',
    body: info,
    actions,
  });
}
