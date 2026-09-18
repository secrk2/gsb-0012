// ========== 人员出勤详情：甘特 + 原始打卡 + 作废/恢复 + 人工改判 + 留痕 ==========
import { api, cachedGet, getUser, isOnline } from '../api.js';
import { el, toast, openModal, fmtTime, dataTimestamp, errorState, skeletonCard } from '../ui.js';
import { CALIBERS, getCaliber, setCaliber, pct, hoursOf, HALF_DAY_NOTE } from '../calibers.js';
import { STATUS_CLS, STATUS_GLYPH } from './attendance.js';

function weekCN(t) { return ['日', '一', '二', '三', '四', '五', '六'][new Date(t + 'T00:00:00').getDay()]; }
function todayStr() {
  const d = new Date();
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}
function shiftWeekAnchor(anchor, n) {
  const d = new Date(anchor + 'T00:00:00');
  d.setDate(d.getDate() + n * 7);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

const ACTION_LABELS = {
  void_punch: '作废打卡', restore_punch: '恢复打卡', adjudicate: '人工改判',
  settle_lock: '结算锁定', unlock: '解除锁定', punch_locked: '冻结月补打卡',
};
const TARGETS = [
  ['present', '出勤'], ['late', '迟到'], ['early', '早退'], ['absent', '缺勤'], ['rest', '休息'],
];

export async function renderAttendanceDetail(root, idOrMe) {
  const me = getUser();
  const isMe = idOrMe === 'me';
  const anchor = (location.hash.split('?')[1] ? new URLSearchParams(location.hash.split('?')[1]).get('from') : '') || todayStr();
  root.innerHTML = '';
  root.append(skeletonCard(420));

  const path = isMe ? '/api/attendance/me?view=week&from=' + anchor
                    : '/api/workers/' + idOrMe + '/attendance?view=week&from=' + anchor;
  let res;
  try {
    res = await cachedGet('att:' + idOrMe + ':' + anchor, path);
  } catch (e) {
    root.innerHTML = '';
    if (e.status === 403) {
      root.append(errorState({ icon: '⛔', title: '无权查看该出勤记录', desc: e.message, backHash: '#/attendance' }));
    } else if (e.status === 404) {
      root.append(errorState({ icon: '🧾', title: '人员不存在', desc: '该人员档案不存在或已被删除。', backHash: '#/attendance' }));
    } else {
      // 空态①：加载失败
      root.append(errorState({
        icon: '📡', title: '出勤详情加载失败',
        desc: e.offline ? '当前离线且本机没有该人员出勤缓存，联网后自动恢复。' : (e.message || '服务暂时不可用。'),
        backHash: '#/attendance',
      }));
    }
    return;
  }
  const d = res.data;
  const w = d.worker;
  const canManage = !!d.can_manage;

  // ---- 头部 ----
  const head = el('div', { class: 'page-head' },
    el('div', {},
      el('h1', {}, '出勤详情'),
      el('div', { class: 'sub' }, `${w.name} · 工号 ${w.job_no} · ${w.team} · ${w.trade || ''}`),
    ),
    el('div', { class: 'actions' },
      dataTimestamp(res.cachedAt, res.stale),
      el('a', { class: 'btn ghost sm', href: isMe ? '#/attendance' : '#/attendance' }, '‹ 返回出勤打卡'),
    ),
  );

  // ---- 规则 + 双口径 ----
  const rule = d.rule || {};
  const caliberBar = el('div', { class: 'at-caliber' });
  function renderCaliber() {
    const cur = getCaliber();
    const t = d.total || {};
    caliberBar.innerHTML = '';
    caliberBar.append(
      el('div', { class: 'card caliber-card' },
        el('div', { class: 'caliber-switch' },
          Object.values(CALIBERS).map(c => el('button', {
            class: 'filter-chip' + (cur === c.key ? ' active' : ''),
            onclick: () => { setCaliber(c.key); renderCaliber(); },
          }, c.label))),
        el('div', { class: 'caliber-def' }, CALIBERS[cur].text + '　=　' + pct(CALIBERS[cur].pick(t))),
        el('div', { class: 'kv-row' },
          el('span', {}, `应出勤天 ${Number(t.scheduled_days || 0).toFixed(1)}`),
          el('span', {}, `实际出勤天 ${Number(t.present_days || 0).toFixed(1)}`),
          el('span', {}, `排班 ${hoursOf(t.scheduled_minutes)}h`),
          el('span', {}, `有效打卡 ${hoursOf(t.worked_minutes)}h`),
        ),
        rule.half_day ? el('div', { class: 'caliber-warn' }, '⚠️ ' + HALF_DAY_NOTE) : null,
        el('div', { class: 'hint' }, `班次：${rule.shift_name || '-'}　上班截止 ${rule.in_deadline || '-'}（晚于算迟到）　下班最早 ${rule.out_deadline || '-'}（早于算早退）`),
      ),
    );
  }
  renderCaliber();

  // ---- 周甘特 ----
  const ganttCard = buildDetailGantt(d, canManage, anchor, () => renderAttendanceDetail(root, idOrMe));

  // ---- 原始打卡（含作废） ----
  const punches = d.punches || [];
  const punchCard = el('div', { class: 'card' },
    el('h3', {}, '原始打卡流水', el('span', { class: 'spacer' }), el('span', { class: 'tag' }, `${punches.length} 条（含已作废）`)),
  );
  if (!punches.length) {
    // 空态②：该人无打卡
    punchCard.append(el('div', { class: 'error-state at-empty' },
      el('div', { class: 'icon' }, '🧾'),
      el('h2', {}, '该时段没有出勤打卡'),
      el('p', {}, '没有闸机或手机定位流水。若确已到岗，可由管理人员补打；多台设备同日打卡会合并成一条出勤。'),
    ));
  } else if (punches.every(p => p.voided)) {
    // 空态③：全作废
    punchCard.append(el('div', { class: 'error-state at-empty' },
      el('div', { class: 'icon' }, '🚫'),
      el('h2', {}, '该时段打卡已全部作废'),
      el('p', {}, '所有流水均被作废（不计出勤/工资）。可逐条「恢复」，恢复会重新合并日结论并留痕。'),
    ));
  } else {
    punchCard.append(el('div', { class: 'list punch-list' }, punches.map(p =>
      el('div', { class: 'list-item punch-item' + (p.voided ? ' voided' : '') },
        el('span', { class: `tag src-tag src-${p.source}` }, p.source_label + (p.device ? '·' + p.device : '')),
        el('div', { class: 'grow' },
          el('div', { class: 'title' }, `${p.date} 周${weekCN(p.date)} · ${p.time}`),
          el('div', { class: 'meta' }, p.voided ? '已作废（不计入日结论）' : '有效'),
        ),
        canManage ? el('div', { class: 'punch-ops' },
          p.voided
            ? el('button', { class: 'btn sm ghost', onclick: () => correctPunch(p, 'restore', () => renderAttendanceDetail(root, idOrMe)) }, '恢复')
            : el('button', { class: 'btn sm danger', onclick: () => correctPunch(p, 'void', () => renderAttendanceDetail(root, idOrMe)) }, '作废'),
        ) : null,
      ))));
  }

  // ---- 改判操作 ----
  let adjudicateCard = null;
  if (canManage) {
    const dayI = el('input', { type: 'date', value: anchor, max: todayStr() });
    const statusI = el('select', {}, TARGETS.map(([k, l]) => el('option', { value: k }, l)));
    adjudicateCard = el('div', { class: 'card' },
      el('h3', {}, '人工改判出勤'),
      el('div', { class: 'field-row' },
        el('div', { class: 'field' }, el('label', {}, '日期'), dayI),
        el('div', { class: 'field' }, el('label', {}, '改为结论'), statusI)),
      el('div', { class: 'hint' }, '改判后该日以人工结论为准，后续打卡不再自动覆盖。若落在已结算月，需二次确认+原因，且不重算当月工资。'),
      el('button', {
        class: 'btn', style: 'margin-top:8px',
        onclick: () => adjudicate(w.id, dayI.value, statusI.value, () => renderAttendanceDetail(root, idOrMe)),
      }, '提交改判'),
      !isOnline() ? el('div', { class: 'masked-note' }, '📡 当前离线：操作暂不可提交，联网后在本页重试。') : null,
    );
  }

  // ---- 留痕 ----
  const corrs = d.corrections || [];
  const corrCard = el('div', { class: 'card' },
    el('h3', {}, '作废 / 改判 / 冻结留痕'),
    corrs.length
      ? el('div', { class: 'timeline' }, corrs.map(c =>
          el('div', { class: 'tl-item' },
            el('div', { class: 'tl-title' }, `${ACTION_LABELS[c.action] || c.action} · ${c.day}`),
            el('div', { class: 'tl-meta' }, `${fmtTime(c.created_at)} · 操作人：${c.actor_name}` + (c.from_label ? `　${c.from_label}→${c.to_label}` : '')),
            el('div', { class: 'tl-reason' }, c.reason),
          )))
      : el('div', { class: 'empty' }, '暂无作废/改判记录'),
  );

  root.innerHTML = '';
  root.append(
    head,
    el('div', { class: 'detail-grid' },
      el('div', {}, caliberBar, ganttCard, adjudicateCard),
      el('div', {}, punchCard, corrCard),
    ),
  );
}

// 周甘特（详情版，可点格子改判）
function buildDetailGantt(d, canManage, anchor, reload) {
  const dates = d.dates || [];
  const today = todayStr();
  const w = d.worker;
  const worker = { worker_id: w.id, name: w.name };
  const headRow = el('div', { class: 'at-row at-head-row' },
    el('div', { class: 'at-name-cell' }, '日期'),
    el('div', { class: 'at-cells' }, dates.map(ds =>
      el('div', { class: 'at-col-head' + (['六', '日'].includes(weekCN(ds)) ? ' wkend' : '') + (ds === today ? ' today' : '') },
        el('div', { class: 'ch-d' }, Number(ds.slice(8))), el('div', { class: 'ch-w' }, weekCN(ds))))),
  );
  const cells = dates.map(ds => {
    const c = d.workers[0].days[ds];
    const cell = el('div', {
      class: 'at-cell ' + (STATUS_CLS[c.status] || 'at-rest') + (ds === today ? ' today' : '') + (c.manual ? ' manual' : '') + (c.locked ? ' locked' : ''),
      title: `${c.label}${c.first_in ? ' · 首入 ' + c.first_in : ''}${c.last_out ? ' 末出 ' + c.last_out : ''}${c.sources ? ' · ' + c.sources : ''}${c.locked ? ' · 已冻结' : ''}`,
      dataset: { date: ds.slice(5), wk: '周' + weekCN(ds), label: c.label },
    }, el('span', { class: 'at-glyph' }, STATUS_GLYPH[c.status] || '·'));
    if (canManage) {
      cell.style.cursor = 'pointer';
      cell.addEventListener('click', () => quickAdjudicate(worker, c, reload));
    }
    return cell;
  });
  const bodyRow = el('div', { class: 'at-row' }, el('div', { class: 'at-name-cell' }, '本周'), el('div', { class: 'at-cells' }, cells));
  const nav = new URLSearchParams(location.hash.split('?')[1] || '');
  return el('div', { class: 'card at-card' },
    el('h3', {}, '本周出勤', el('span', { class: 'spacer' }),
      el('a', { class: 'btn sm ghost', href: `#/attendance/${w.id}?from=${shiftWeekAnchor(anchor, -1)}` }, '‹ 上周'),
      el('a', { class: 'btn sm ghost', href: `#/attendance/${w.id}?from=${shiftWeekAnchor(anchor, 1)}` }, '下周 ›')),
    el('div', { class: 'at-scroll' }, el('div', { class: 'at-grid single' }, headRow, bodyRow)),
  );
}

// ---------- 作废 / 恢复（冻结月二次确认 + 原因） ----------
function correctPunch(p, mode, reload) {
  const reasonI = el('input', { type: 'text', placeholder: mode === 'void' ? '如：代打卡嫌疑，作废待核' : '如：核实为本人，恢复有效' });
  const errBox = el('div', { class: 'login-error' });
  const isVoid = mode === 'void';

  async function submit(confirm) {
    errBox.textContent = '';
    if (!reasonI.value.trim()) { errBox.textContent = '请填写原因（操作将留痕）'; return; }
    try {
      await api.post(`/api/attendance/punches/${p.id}/` + (isVoid ? 'void' : 'restore'), {
        reason: reasonI.value, confirm,
      });
      close();
      toast(isVoid ? '已作废，日结论已重算并留痕' : '已恢复，日结论已重算并留痕');
      reload();
    } catch (e) {
      if (e.status === 409 && e.data && e.data.need_confirm) {
        openModal({
          title: '二次确认：改动已结算月份',
          sub: e.data.error,
          body: el('div', {}, el('div', { class: 'warn-box' }, '当月工资冻结不变；如确有差额请到分账页登记「差额调整」。'),
            el('div', { class: 'field' }, el('label', {}, '原因（必填）'), reasonI)),
          actions: [
            { label: '取消', kind: 'ghost' },
            { label: '确认并留痕', kind: 'danger', onClick: (c2) => { c2(); submit(true); } },
          ],
        });
        return;
      }
      errBox.textContent = e.message;
    }
  }
  const close = openModal({
    title: isVoid ? '作废该条打卡？' : '恢复该条打卡？',
    sub: `${p.date} ${p.time} · ${p.source_label}${p.device ? '·' + p.device : ''}`,
    body: el('div', {},
      el('div', { class: 'warn-box' }, isVoid ? '作废后该条不计入当日合并结论；若当日全部作废，日结论变为「已作废」。' : '恢复后将重新参与当日多源合并判定。'),
      el('div', { class: 'field' }, el('label', {}, '原因（必填，留痕）'), reasonI), errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      { label: isVoid ? '确认作废' : '确认恢复', kind: isVoid ? 'danger' : 'primary', onClick: () => submit(false) },
    ],
  });
}

// ---------- 改判（列表按钮 & 甘特格子快捷） ----------
function adjudicate(workerId, day, status, reload, presetCell) {
  const reasonI = el('textarea', { rows: '3', placeholder: '请填写改判依据（必填，留痕），如：现场点名确认在岗' });
  const statusI = el('select', {}, TARGETS.map(([k, l]) =>
    el('option', { value: k, selected: presetCell && k === status ? '' : null }, l)));
  const errBox = el('div', { class: 'login-error' });
  const targetDay = presetCell ? presetCell.day : day;

  async function submit(confirm) {
    errBox.textContent = '';
    if (!reasonI.value.trim()) { errBox.textContent = '请填写改判原因（将留痕）'; return; }
    try {
      await api.post('/api/attendance/adjudicate', {
        worker_id: workerId, day: targetDay, status: statusI.value,
        reason: reasonI.value, confirm,
      });
      close();
      toast('已改判并留痕');
      reload();
    } catch (e) {
      if (e.status === 409 && e.data && e.data.need_confirm) {
        openModal({
          title: '二次确认：改判已结算月份',
          sub: e.data.error,
          body: el('div', {}, el('div', { class: 'warn-box' }, '当月工资冻结不变；改判只影响出勤记录与后续月份。'),
            el('div', { class: 'field' }, el('label', {}, '改判原因（必填）'), reasonI)),
          actions: [
            { label: '取消', kind: 'ghost' },
            { label: '确认改判并留痕', kind: 'danger', onClick: (c2) => { c2(); submit(true); } },
          ],
        });
        return;
      }
      errBox.textContent = e.message;
    }
  }
  const close = openModal({
    title: '人工改判出勤',
    sub: `${targetDay}`,
    body: el('div', {},
      el('div', { class: 'field' }, el('label', {}, '改判结论'), statusI),
      el('div', { class: 'field' }, el('label', {}, '原因（必填）'), reasonI),
      presetCell ? el('div', { class: 'hint' }, `当前系统结论：${presetCell.label}；首入 ${presetCell.first_in || '-'}，末出 ${presetCell.last_out || '-'}，来源 ${presetCell.sources || '-'}`) : null,
      errBox),
    actions: [
      { label: '取消', kind: 'ghost' },
      { label: '确认改判', kind: 'primary', onClick: () => submit(false) },
    ],
  });
}

function quickAdjudicate(worker, c, reload) {
  openModal({
    title: `${c.day} · ${c.label}`,
    sub: c.locked ? '已结算冻结月：改动需二次确认+原因，不重算当月工资' : '',
    body: el('div', {},
      el('dl', { class: 'kv' },
        el('dt', {}, '首入/末出'), el('dd', {}, `${c.first_in || '-'} / ${c.last_out || '-'}`),
        el('dt', {}, '合并来源'), el('dd', {}, c.sources ? c.sources + '（已合并为一条）' : '-'),
        el('dt', {}, '有效打卡'), el('dd', {}, `${c.valid_punches ?? c.valid} / ${c.total_punches ?? c.total}`),
        el('dt', {}, '有效工时'), el('dd', {}, hoursOf(c.work_minutes) + 'h'),
      ),
    ),
    actions: [
      { label: '关闭', kind: 'ghost' },
      { label: '人工改判此日', kind: 'primary', onClick: (close) => { close(); adjudicate(worker.worker_id, c.day, c.status === 'rest' ? 'present' : c.status, reload, c); } },
    ],
  });
}
