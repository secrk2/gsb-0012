// ========== 出勤打卡甘特（周/月）+ 逐日打卡详情 + 作废/恢复 ==========
import { api, cachedGet, submitPunch, getUser, isOnline } from '../api.js';
import { el, toast, openModal, fmtTime, dataTimestamp, errorState, skeletonCard } from '../ui.js';
import { caliber, fmtPct } from '../calibers.js';

const WEEKDAYS = ['日', '一', '二', '三', '四', '五', '六'];
const CALIBER_KEY = 'gt_caliber';

const CELL_META = {
  present:    { cls: 'at-present', txt: '勤' },
  late:       { cls: 'at-late', txt: '迟' },
  early:      { cls: 'at-early', txt: '早' },
  late_early: { cls: 'at-lateearly', txt: '迟早' },
  absent:     { cls: 'at-absent', txt: '缺' },
  leave:      { cls: 'at-leave', txt: '假' },
  voided:     { cls: 'at-voided', txt: '废' },
  excluded:   { cls: 'at-excluded', txt: '' },
};

export async function renderAttendance(root, params) {
  const me = getUser();
  const view = params.get('view') === 'month' ? 'month' : 'week';
  const anchor = params.get(view === 'month' ? 'month' : 'date') || '';
  const team = me.role === 'sub_leader' ? me.team : (params.get('team') || '');
  const calKey = (params.get('caliber') || localStorage.getItem(CALIBER_KEY) || 'days');
  localStorage.setItem(CALIBER_KEY, calKey);

  root.innerHTML = '';
  root.append(skeletonCard(120), el('div', { style: 'height:12px' }), skeletonCard(420));

  // 监管员可切换工地
  let sites = [];
  if (me.role === 'regulator') {
    try {
      const sr = await cachedGet('sites', '/api/sites');
      sites = sr.data.sites || [];
    } catch { sites = []; }
  }
  const siteId = params.get('site_id') || (sites[0] ? String(sites[0].id) : '');

  const qs = new URLSearchParams();
  qs.set('view', view);
  if (view === 'month') qs.set('month', anchor || todayMonth());
  else if (anchor) qs.set('date', anchor);
  if (team) qs.set('team', team);
  if (me.role === 'regulator' && siteId) qs.set('site_id', siteId);
  qs.set('caliber', calKey);

  let res;
  try {
    res = await cachedGet('gantt:' + qs.toString(), '/api/attendance/gantt?' + qs.toString());
  } catch (e) {
    root.innerHTML = '';
    // 空态①：加载失败（离线且无缓存 / 服务错误）——不与业务空态混用
    root.append(errorState({
      icon: '📡',
      title: '出勤打卡数据加载失败',
      desc: e.offline ? '当前离线且本地没有缓存数据。打卡仍可先在手机端排队，联网后自动同步。' : (e.message || '请稍后重试'),
      backHash: '#/dashboard',
      backLabel: '返回作战台',
    }), el('div', { style: 'text-align:center;margin-top:-40px' },
      el('button', { class: 'btn', onclick: () => renderAttendance(root, params) }, '重新加载')));
    return;
  }
  const d = res.data;
  const activeCal = caliber(calKey);

  // ---------- 工具栏 ----------
  const periodLabel = view === 'month'
    ? d.from.slice(0, 7) + ' 月'
    : d.from.slice(5) + ' ~ ' + d.to.slice(5);

  const goto = (newAnchor) => {
    const p = new URLSearchParams();
    p.set('view', view);
    p.set(view === 'month' ? 'month' : 'date', newAnchor);
    if (team) p.set('team', team);
    if (me.role === 'regulator' && params.get('site_id')) p.set('site_id', params.get('site_id'));
    p.set('caliber', calKey);
    location.hash = '#/attendance?' + p.toString();
  };

  const shift = (dir) => {
    if (view === 'month') {
      const t = new Date((anchor || todayMonth()) + '-01T00:00:00');
      t.setMonth(t.getMonth() + dir);
      goto(t.getFullYear() + '-' + String(t.getMonth() + 1).padStart(2, '0'));
    } else {
      const base = anchor ? new Date(anchor + 'T00:00:00') : new Date();
      base.setDate(base.getDate() + dir * 7);
      goto(base.toISOString().slice(0, 10));
    }
  };

  const siteSel = me.role === 'regulator' && sites.length > 1
    ? el('select', {
        class: 'search-input',
        onchange: (e) => {
          const p = new URLSearchParams(params.toString());
          p.set('site_id', e.target.value);
          location.hash = '#/attendance?' + p.toString();
        },
      }, sites.map(s => el('option', { value: String(s.id), selected: String(s.id) === siteId ? '' : null }, s.name)))
    : null;

  const teamSel = (d.teams || []).length > 1 && me.role !== 'sub_leader' && me.role !== 'worker'
    ? el('select', {
        class: 'search-input',
        onchange: (e) => {
          const p = new URLSearchParams(params.toString());
          if (e.target.value) p.set('team', e.target.value); else p.delete('team');
          location.hash = '#/attendance?' + p.toString();
        },
      }, [el('option', { value: '', selected: team ? null : '' }, '全部班组')]
        .concat(d.teams.map(t => el('option', { value: t, selected: t === team ? '' : null }, t))))
    : null;

  const exportBtn = me.role !== 'worker'
    ? el('button', { class: 'btn sm ghost', onclick: () => downloadCsv(qs) }, '⬇ 导出CSV')
    : null;

  const head = el('div', { class: 'page-head' },
    el('div', {},
      el('h1', {}, '出勤打卡'),
      el('div', { class: 'sub' }, '闸机/手机多端打卡自动合并为每人每天一条 · 工资按出勤工日×日单价'),
    ),
    el('div', { class: 'actions' }, siteSel, teamSel, exportBtn, dataTimestamp(res.cachedAt, res.stale)),
  );

  const viewSwitch = el('div', { class: 'seg' },
    el('a', { class: 'seg-item' + (view === 'week' ? ' active' : ''), href: hashSwitch(params, 'week', d.from) }, '周甘特'),
    el('a', { class: 'seg-item' + (view === 'month' ? ' active' : ''), href: hashSwitch(params, 'month', d.from.slice(0, 7)) }, '月甘特'),
  );
  const pager = el('div', { class: 'pager' },
    el('button', { class: 'btn sm ghost', onclick: () => shift(-1) }, '‹ 上' + (view === 'month' ? '月' : '周')),
    el('span', { class: 'pager-label' }, periodLabel),
    el('button', { class: 'btn sm ghost', onclick: () => shift(1) }, '下' + (view === 'month' ? '月' : '周') + ' ›'),
    el('button', { class: 'btn sm', onclick: () => goto(view === 'month' ? todayMonth() : '') }, '回到本周'),
  );

  // ---------- 口径切换：公式直接写在界面 ----------
  const calSwitch = el('div', { class: 'caliber-box' },
    el('div', { class: 'caliber-tabs' },
      ...['days', 'hours'].map(k => el('button', {
        class: 'seg-item' + (k === calKey ? ' active' : ''),
        onclick: () => { localStorage.setItem(CALIBER_KEY, k); location.hash = hashWith(params, { caliber: k }); },
      }, caliber(k).label + (k === 'days' ? '（出勤天/应出勤天）' : '（打卡工时/排班工时）'))),
    ),
    el('div', { class: 'caliber-formula' }, '📐 ' + activeCal.formula),
    el('div', { class: 'caliber-note' }, activeCal.note),
  );

  // ---------- 汇总 ----------
  const t = d.totals || {};
  const totalRate = calKey === 'days' ? t.rate_days_pct : t.rate_hours_pct;
  const otherRate = calKey === 'days' ? t.rate_hours_pct : t.rate_days_pct;
  const summary = el('div', { class: 'stat-row att-stats' },
    statCard(fmtPct(totalRate), '出勤率 · ' + activeCal.label, 'c-onsite'),
    statCard(fmtPct(otherRate), '另一口径 · ' + caliber(calKey === 'days' ? 'hours' : 'days').label, 'c-pending'),
    statCard((t.present_days || 0) + ' 天', '实际出勤（迟到/早退仍算出勤）', 'c-ok'),
    statCard((t.late_days || 0) + ' / ' + (t.early_days || 0), '迟到 / 早退人天', 'c-leave'),
    statCard((t.absent_days || 0) + ' 天', '缺勤（含打卡作废）', 'c-alert'),
    statCard((t.workdays || 0).toFixed(1), '出勤工日合计（计薪基数）', 'c-hazard'),
  );

  const legend = el('div', { class: 'legend' },
    ...[['present', '出勤'], ['late', '迟到'], ['early', '早退'], ['late_early', '迟到+早退'],
      ['absent', '缺勤'], ['leave', '请假'], ['voided', '打卡作废'], ['excluded', '非排班']]
      .map(([k, label]) => el('span', { class: 'legend-item' }, el('i', { class: 'at-cell ' + CELL_META[k].cls }), label)),
    el('span', { class: 'legend-item dim' }, '点击任意日期格可看当日打卡明细/作废'),
  );

  // ---------- 工人本人：手机打卡 ----------
  const punchBar = me.role === 'worker' ? selfPunchBar(() => renderAttendance(root, params)) : null;

  // ---------- 三端甘特 ----------
  const people = d.people || [];
  const noPunchPeople = people.filter(p => !p.has_any_punch);
  const allVoidPeople = people.filter(p => p.all_void);

  const desktop = el('div', { class: 'gantt-desktop' }, buildDesktopTable(d, people, calKey, openDay));
  let selectedId = people.length ? people[0].worker_id : null;
  const tablet = el('div', { class: 'gantt-tablet' });
  const phone = el('div', { class: 'gantt-phone' });

  function renderResponsive() {
    tablet.innerHTML = '';
    phone.innerHTML = '';
    tablet.append(buildTablet(d, people, calKey, selectedId, (id) => { selectedId = id; renderResponsive(); }, openDay));
    phone.append(...buildPhone(d, people, calKey, openDay));
  }
  renderResponsive();

  function openDay(person, date) {
    openDayModal(me, person, date, d.caliber ? d.caliber : activeCal, () => renderAttendance(root, params));
  }

  // 空态②/③：该人无打卡、全部作废（与加载失败严格分开）
  const emptyNotice = el('div', { class: 'att-empty-row' });
  const empties = [];
  if (noPunchPeople.length) {
    empties.push(el('div', { class: 'att-empty nopunch' },
      el('div', { class: 'ae-icon' }, '🚫'),
      el('div', { class: 'ae-body' },
        el('div', { class: 'ae-title' }, `该时段有 ${noPunchPeople.length} 人无任何出勤打卡`),
        el('div', { class: 'ae-desc' }, noPunchPeople.map(p => p.job_no + ' ' + p.name).join('、') + '。可能未入场或闸机/手机均未上报，请核实。'),
      )));
  }
  if (allVoidPeople.length) {
    empties.push(el('div', { class: 'att-empty allvoid' },
      el('div', { class: 'ae-icon' }, '🛑'),
      el('div', { class: 'ae-body' },
        el('div', { class: 'ae-title' }, `${allVoidPeople.length} 人的打卡在该时段已全部作废`),
        el('div', { class: 'ae-desc' }, allVoidPeople.map(p => p.job_no + ' ' + p.name).join('、') + '。作废按缺勤处理；如系误判，可点对应日期格恢复，恢复后自动重算。'),
      )));
  }
  emptyNotice.append(...empties);

  root.innerHTML = '';
  root.append(
    head,
    el('div', { class: 'toolbar' }, viewSwitch, pager),
    calSwitch,
    punchBar,
    summary,
    legend,
    desktop, tablet, phone,
    emptyNotice,
  );
}

function statCard(num, label, cls) {
  return el('div', { class: 'stat-card ' + cls }, el('div', { class: 'num' }, num), el('div', { class: 'label' }, label));
}

function todayMonth() {
  const t = new Date();
  return t.getFullYear() + '-' + String(t.getMonth() + 1).padStart(2, '0');
}

function hashWith(params, patch) {
  const p = new URLSearchParams(params.toString());
  for (const [k, v] of Object.entries(patch)) {
    if (!v) p.delete(k); else p.set(k, v);
  }
  return '#/attendance?' + p.toString();
}

// 周/月切换：清掉另一视图的锚点参数，避免 month/date 串台
function hashSwitch(params, view, anchor) {
  const p = new URLSearchParams(params.toString());
  p.delete('date');
  p.delete('month');
  p.set('view', view);
  p.set(view === 'month' ? 'month' : 'date', anchor);
  return '#/attendance?' + p.toString();
}

// ---------- 桌面：整表甘特（左列吸住，横向滚动） ----------
function buildDesktopTable(d, people, calKey, openDay) {
  const dates = d.dates || [];
  const headRow = el('tr', {},
    el('th', { class: 'at-name-col' }, '人员 / 班组'),
    ...dates.map(dt => {
      const d0 = new Date(dt + 'T00:00:00');
      return el('th', { class: d0.getDay() === 0 ? 'col-sunday' : '' },
        el('div', { class: 'at-d' }, dt.slice(5)),
        el('div', { class: 'at-w' }, WEEKDAYS[d0.getDay()]));
    }),
    el('th', { class: 'at-sum-col' }, '出勤率'),
  );
  const body = people.map(p => el('tr', {},
    el('td', { class: 'at-name-col' },
      el('div', { class: 'at-pname' }, p.name, p.all_void ? el('span', { class: 'mini-badge danger' }, '全作废') : (!p.has_any_punch ? el('span', { class: 'mini-badge' }, '无打卡') : null)),
      el('div', { class: 'at-pmeta' }, `${p.job_no} · ${p.team} · ${p.day_type_label}`),
    ),
    ...dates.map(dt => cellTd(p, dt, openDay)),
    el('td', { class: 'at-sum-col' }, rateBlock(p, calKey)),
  ));
  return el('div', { class: 'at-table-scroll' },
    el('table', { class: 'at-table' }, el('thead', {}, headRow), el('tbody', {}, body)));
}

function cellTd(p, dt, openDay) {
  const c = p.cells[dt];
  const meta = CELL_META[c.status] || CELL_META.excluded;
  const isSunday = new Date(dt + 'T00:00:00').getDay() === 0;
  const td = el('td', { class: 'at-cell-td' + (isSunday ? ' col-sunday' : '') },
    el('button', {
      class: 'at-cell ' + meta.cls + (c.merged ? ' merged' : ''),
      title: cellTitle(p, c, dt),
      onclick: () => openDay({ worker_id: p.worker_id, job_no: p.job_no, name: p.name, team: p.team }, dt),
    }, meta.txt),
  );
  return td;
}

function cellTitle(p, c, dt) {
  const parts = [`${p.job_no} ${p.name} ${dt}`, c.status_label];
  if (c.first_in) parts.push(`首卡 ${c.first_in}`);
  if (c.last_out) parts.push(`末卡 ${c.last_out}`);
  if (c.hours) parts.push(`有效工时 ${c.hours}h`);
  if (c.note) parts.push(c.note);
  return parts.join('\n');
}

function rateBlock(p, calKey) {
  const main = calKey === 'days' ? p.rate_days_pct : p.rate_hours_pct;
  const sub = calKey === 'days' ? p.rate_hours_pct : p.rate_days_pct;
  return el('div', { class: 'at-rate' },
    el('div', { class: 'at-rate-main' }, fmtPct(main)),
    el('div', { class: 'at-rate-sub' }, '另一口径 ' + fmtPct(sub)),
    el('div', { class: 'at-rate-meta' }, `迟${p.late_days} 早${p.early_days} 缺${p.absent_days} 废${p.voided_days}`),
  );
}

// ---------- 平板：左名单 + 右单人甘特条 ----------
function buildTablet(d, people, calKey, selectedId, onSelect, openDay) {
  const dates = d.dates || [];
  const list = el('div', { class: 'at-person-list' }, people.map(p =>
    el('button', {
      class: 'at-person-item' + (p.worker_id === selectedId ? ' active' : ''),
      onclick: () => onSelect(p.worker_id),
    },
      el('div', { class: 'at-pname' }, p.name, p.all_void ? el('span', { class: 'mini-badge danger' }, '废') : (!p.has_any_punch ? el('span', { class: 'mini-badge' }, '无') : null)),
      el('div', { class: 'at-pmeta' }, p.job_no),
      el('div', { class: 'at-person-rate' }, fmtPct(calKey === 'days' ? p.rate_days_pct : p.rate_hours_pct)),
    )));

  const p = people.find(x => x.worker_id === selectedId);
  const strip = el('div', { class: 'at-strip' });
  if (!p) {
    strip.append(el('div', { class: 'empty' }, '请选择左侧人员'));
  } else {
    strip.append(
      el('div', { class: 'at-strip-head' },
        el('div', {}, el('div', { class: 'at-pname' }, p.name), el('div', { class: 'at-pmeta' }, `${p.job_no} · ${p.team} · ${p.day_type_label}`)),
        rateBlock(p, calKey),
      ),
      el('div', { class: 'at-strip-grid' }, dates.map(dt => {
        const c = p.cells[dt];
        const meta = CELL_META[c.status] || CELL_META.excluded;
        const d0 = new Date(dt + 'T00:00:00');
        return el('button', {
          class: 'at-strip-cell ' + meta.cls + (c.merged ? ' merged' : ''),
          title: cellTitle(p, c, dt),
          onclick: () => openDay({ worker_id: p.worker_id, job_no: p.job_no, name: p.name, team: p.team }, dt),
        },
          el('span', { class: 'asc-d' }, dt.slice(5)),
          el('span', { class: 'asc-w' }, WEEKDAYS[d0.getDay()]),
          el('span', { class: 'asc-t' }, meta.txt || '·'),
          c.hours ? el('span', { class: 'asc-h' }, c.hours + 'h') : null);
      })),
      personEmptyLine(p),
    );
  }
  return el('div', { class: 'gantt-two-pane' }, list, el('div', { class: 'at-strip-wrap' }, strip));
}

// ---------- 手机：竖向人员卡片 ----------
function buildPhone(d, people, calKey, openDay) {
  const dates = d.dates || [];
  return people.map(p => {
    const rate = calKey === 'days' ? p.rate_days_pct : p.rate_hours_pct;
    const other = calKey === 'days' ? p.rate_hours_pct : p.rate_days_pct;
    return el('div', { class: 'card at-phone-card' + (p.all_void ? ' all-void' : '') },
      el('div', { class: 'atp-head' },
        el('div', {},
          el('div', { class: 'at-pname' }, p.name),
          el('div', { class: 'at-pmeta' }, `${p.job_no} · ${p.day_type_label}`),
        ),
        el('div', { class: 'atp-rate' },
          el('div', { class: 'at-rate-main' }, fmtPct(rate)),
          el('div', { class: 'at-rate-sub' }, '另一口径 ' + fmtPct(other)),
        ),
      ),
      // 空态②：该人无打卡
      !p.has_any_punch
        ? el('div', { class: 'att-empty compact' },
            el('div', { class: 'ae-icon' }, '🚫'),
            el('div', { class: 'ae-body' },
              el('div', { class: 'ae-title' }, '该时段无任何出勤打卡'),
              el('div', { class: 'ae-desc' }, '闸机与手机定位均无记录。如确有出勤，请联系班组长核实补卡。')))
        : el('div', { class: 'atp-grid' }, dates.map(dt => {
            const c = p.cells[dt];
            const meta = CELL_META[c.status] || CELL_META.excluded;
            const d0 = new Date(dt + 'T00:00:00');
            return el('button', {
              class: 'atp-cell ' + meta.cls + (c.merged ? ' merged' : ''),
              title: cellTitle(p, c, dt),
              onclick: () => openDay({ worker_id: p.worker_id, job_no: p.job_no, name: p.name, team: p.team }, dt),
            },
              el('span', { class: 'asc-d' }, dt.slice(5)),
              el('span', { class: 'asc-w' }, WEEKDAYS[d0.getDay()]),
              el('span', { class: 'asc-t' }, meta.txt || '·'));
          })),
      // 空态③：全部作废
      p.all_void && p.has_any_punch
        ? el('div', { class: 'att-empty compact allvoid' },
            el('div', { class: 'ae-icon' }, '🛑'),
            el('div', { class: 'ae-body' },
              el('div', { class: 'ae-title' }, '该时段打卡已全部作废'),
              el('div', { class: 'ae-desc' }, '已按缺勤处理；点开日期格可查看作废原因或申请恢复。')))
        : null,
      el('div', { class: 'atp-meta' },
        `迟 ${p.late_days} · 早 ${p.early_days} · 缺 ${p.absent_days} · 废 ${p.voided_days} · 工日 ${p.workdays}`,
        p.has_any_punch && !p.all_void ? el('span', { class: 'dim' }, '（点日期格看明细）') : null),
    );
  });
}

function personEmptyLine(p) {
  if (p.all_void && p.has_any_punch) {
    return el('div', { class: 'att-empty compact allvoid' },
      el('div', { class: 'ae-icon' }, '🛑'),
      el('div', { class: 'ae-body' }, el('div', { class: 'ae-title' }, '该时段打卡已全部作废'),
        el('div', { class: 'ae-desc' }, '按缺勤处理，可点日期格恢复。')));
  }
  if (!p.has_any_punch) {
    return el('div', { class: 'att-empty compact' },
      el('div', { class: 'ae-icon' }, '🚫'),
      el('div', { class: 'ae-body' }, el('div', { class: 'ae-title' }, '该时段无任何出勤打卡'),
        el('div', { class: 'ae-desc' }, '闸机与手机定位均无记录。')));
  }
  return null;
}

// ---------- 工人本人手机打卡 ----------
function selfPunchBar(onDone) {
  const errBox = el('div', { class: 'login-error' });
  const doPunch = async (direction) => {
    errBox.textContent = '';
    const payload = { direction, source: 'mobile', device_id: 'MOBILE' };
    const useGeo = () => new Promise((resolve) => {
      if (!navigator.geolocation) return resolve(null);
      navigator.geolocation.getCurrentPosition(
        (pos) => resolve({ lat: pos.coords.latitude, lng: pos.coords.longitude, accuracy: pos.coords.accuracy }),
        () => resolve(null), { timeout: 6000 });
    });
    const geo = await useGeo();
    if (geo) Object.assign(payload, geo);
    try {
      const r = await submitPunch(payload);
      if (r.queued) toast('已离线排队，联网后自动同步（多端打卡会合并为一条）', 'warn', 3400);
      else if (r.punch && r.punch.duplicate) toast(r.notice || '重复打卡已合并', 'warn');
      else toast(direction === 'in' ? '上班打卡成功' : '下班打卡成功');
      onDone();
    } catch (e) {
      errBox.textContent = e.message;
    }
  };
  return el('div', { class: 'card punch-bar' },
    el('div', { class: 'pb-info' },
      el('div', { class: 'pb-title' }, '手机定位打卡'),
      el('div', { class: 'pb-desc' }, (isOnline() ? '定位上报本人位置，闸机同日打卡会自动合并为一条' : '当前离线，打卡先排队，联网自动补传')),
    ),
    el('div', { class: 'pb-actions' },
      el('button', { class: 'btn primary', onclick: () => doPunch('in') }, '上班打卡'),
      el('button', { class: 'btn', onclick: () => doPunch('out') }, '下班打卡'),
    ),
    errBox,
  );
}

// ---------- 当日打卡明细 + 作废/恢复（已结算月二次确认+原因+留痕） ----------
async function openDayModal(me, person, date, activeCaliber, onChanged) {
  let close;
  const body = el('div', {}, el('div', { class: 'skeleton', style: 'height:120px' }));
  close = openModal({
    title: `${person.name}（${person.job_no}） ${date.slice(5)} 打卡明细`,
    sub: '同日多台闸机/手机+闸机的打卡自动合并为一条出勤记录',
    body,
    actions: [{ label: '关闭', kind: 'ghost' }],
  });

  let res;
  try {
    res = await api.get(`/api/workers/${person.worker_id}/punches?date=${date}`);
  } catch (e) {
    body.innerHTML = '';
    body.append(errorState({ icon: '📡', title: '明细加载失败', desc: e.message, backHash: location.hash, backLabel: '关闭' }));
    return;
  }
  const d = res;
  body.innerHTML = '';

  const valid = (d.punches || []).filter(p => p.status === 'valid');
  const voided = (d.punches || []).filter(p => p.status === 'void');
  const sources = [...new Set(valid.map(p => p.source_label))];
  const firstIn = valid.filter(p => p.direction === 'in').sort((a, b) => a.punch_time.localeCompare(b.punch_time))[0];
  const lastOut = valid.filter(p => p.direction === 'out').sort((a, b) => b.punch_time.localeCompare(a.punch_time))[0];

  // 当日判定摘要
  const summaryLine = valid.length
    ? el('div', { class: 'day-summary merged' },
        el('div', {}, `合并为 1 条 · 来源：${sources.join(' + ') || '—'}`),
        el('div', { class: 'dim' }, `首卡 ${firstIn ? fmtTime(firstIn.punch_time).slice(11) : '缺'} ｜ 末卡 ${lastOut ? fmtTime(lastOut.punch_time).slice(11) : '缺'}`))
    : el('div', { class: 'day-summary ' + (voided.length ? 'allvoid' : 'none') },
        voided.length ? '🛑 当日打卡已全部作废（按缺勤处理）' : '🚫 当日没有任何打卡记录（闸机/手机均未上报）');
  body.append(summaryLine);

  if (d.month_settled) {
    body.append(el('div', { class: 'warn-box' },
      `⚠️ ${d.month} 月已结算冻结：作废/恢复必须填写原因并二次确认，原工资金额与已发工资不变，差异进入复核补差，全程留痕。`));
  }

  const list = el('div', { class: 'punch-list' }, (d.punches || []).map(p =>
    el('div', { class: 'punch-row ' + p.status },
      el('div', { class: 'grow' },
        el('div', { class: 'title' },
          p.direction_label,
          el('span', { class: 'tag' }, p.source_label),
          el('span', { class: 'tag' }, p.device_id.replace(/^(gate|mobile):/, '')),
          p.status === 'void' ? el('span', { class: 'tag lv-critical' }, '已作废') : null,
        ),
        el('div', { class: 'meta' }, fmtTime(p.punch_time) + (p.accuracy ? ` · 定位精度 ${Math.round(p.accuracy)}m` : '')),
        p.status === 'void'
          ? el('div', { class: 'void-reason' }, `作废：${p.voided_reason}（${p.voided_by_name} · ${fmtTime(p.voided_at)}）`)
          : null,
      ),
      p.can_void
        ? el('button', {
            class: 'btn sm ' + (p.status === 'void' ? '' : 'danger'),
            onclick: () => openVoidModal(p, d.month_settled, close, onChanged),
          }, p.status === 'void' ? '恢复' : '作废')
        : null,
    )));
  if (!d.punches || !d.punches.length) {
    list.append(el('div', { class: 'empty' }, '无打卡流水'));
  }
  body.append(list);
  body.append(el('div', { class: 'caliber-note' }, '📐 出勤率口径（与甘特/导出一致）：' + activeCaliber.formula));
}

function openVoidModal(p, monthSettled, closeDay, onChanged) {
  const restore = p.status === 'void';
  const reasonInput = el('textarea', { rows: '3', placeholder: monthSettled
    ? '该月已结算冻结，请如实填写改动原因（必填，留痕并进入工资复核）'
    : '请填写作废/恢复原因（必填，将留痕）' });
  const confirmChk = el('input', { type: 'checkbox' });
  const errBox = el('div', { class: 'login-error' });

  const submit = async () => {
    errBox.textContent = '';
    const reason = reasonInput.value.trim();
    if (reason.length < 2) { errBox.textContent = '原因不少于2个字'; return; }
    try {
      const r = await api.post(`/api/punches/${p.id}/` + (restore ? 'restore' : 'void'), {
        reason, confirm_settled: confirmChk.checked,
      });
      toast(r.message || (restore ? '已恢复' : '已作废'), monthSettled ? 'warn' : 'ok', 4000);
      closeModal2(); closeDay(); onChanged && onChanged();
    } catch (e) {
      if (e.status === 409 && e.data && e.data.need_confirm) {
        confirmWrap.classList.remove('hidden');
        errBox.textContent = e.message + '。请勾选二次确认后再次提交。';
      } else {
        errBox.textContent = e.message;
      }
    }
  };

  const confirmWrap = el('div', { class: 'field hidden' },
    el('label', { class: 'checkbox-row' }, confirmChk,
      `我已确认：改动 ${p.punch_time ? p.punch_time.slice(0, 10) : ''} 的打卡会影响已结算工资，原金额与已发部分不变，差额走复核补差`));

  let closeModal2;
  closeModal2 = openModal({
    title: restore ? '恢复打卡' : '作废打卡',
    sub: monthSettled ? '该月已结算冻结 · 需二次确认' : '操作将留痕',
    body: el('div', {},
      monthSettled ? el('div', { class: 'warn-box' }, '⚠️ 该打卡属于已结算月份：不能直接冲销工资，仅能标记差异并由总包复核补差。') : null,
      el('div', { class: 'field' }, el('label', {}, '原因（必填）'), reasonInput),
      confirmWrap,
      errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      { label: restore ? '确认恢复' : '确认作废', kind: restore ? 'primary' : 'danger', onClick: () => submit() },
    ],
  });
}

// ---------- 导出（带 Bearer，拉 blob 下载） ----------
async function downloadCsv(qs) {
  try {
    const token = localStorage.getItem('gt_token');
    const resp = await fetch('/api/attendance/export?' + qs.toString(), { headers: { Authorization: 'Bearer ' + token } });
    if (!resp.ok) {
      const txt = await resp.text();
      throw new Error(txt);
    }
    const blob = await resp.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'attendance.csv';
    a.click();
    URL.revokeObjectURL(a.href);
    toast('已按当前口径导出 CSV（表头含口径公式）');
  } catch (e) {
    toast('导出失败：' + e.message, 'err', 3600);
  }
}
