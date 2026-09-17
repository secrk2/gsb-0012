// ========== 工地作战台 ==========
import { api, cachedGet, getUser } from '../api.js';
import { el, toast, statusChip, fmtTime, fmtAgo, dataTimestamp, errorState, skeletonCard, ALERT_TYPE_LABELS } from '../ui.js';
import { CALIBERS, fmtPct } from '../calibers.js';

const FUNNEL_STAGES = [
  ['pending', '待入场'],
  ['onsite', '在场'],
  ['leave', '请假离场'],
  ['exited', '退场'],
  ['blacklisted', '黑名单'],
];

export async function renderDashboard(root, params) {
  const user = getUser();
  root.innerHTML = '';
  root.append(
    skeletonCard(60), el('div', { style: 'height:12px' }),
    skeletonCard(110), el('div', { style: 'height:12px' }),
    skeletonCard(260),
  );

  // 工地列表（监管员可切换）
  let sites = [];
  try {
    const res = await cachedGet('sites', '/api/sites');
    sites = res.data.sites || [];
  } catch (e) {
    root.innerHTML = '';
    root.append(errorState({ icon: '📡', title: '工地数据加载失败', desc: e.offline ? '当前离线且没有缓存数据，请联网后重试。' : e.message }));
    return;
  }
  if (!sites.length) {
    root.innerHTML = '';
    root.append(errorState({ icon: '🏗️', title: '暂无工地', desc: '当前账号未绑定任何工地。' }));
    return;
  }
  const siteId = params.get('site_id') || String(sites[0].id);

  let dashRes;
  try {
    dashRes = await cachedGet('dashboard:' + siteId, '/api/dashboard?site_id=' + siteId);
  } catch (e) {
    root.innerHTML = '';
    if (e.status === 403) {
      root.append(errorState({ icon: '⛔', title: '无权查看该工地', desc: e.message }));
    } else {
      root.append(errorState({ icon: '📡', title: '作战台数据加载失败', desc: e.offline ? '当前离线且没有缓存数据，请联网后重试。' : e.message }));
    }
    return;
  }
  const d = dashRes.data;
  const funnel = d.funnel || {};

  // ---- 页头 ----
  const siteSelector = sites.length > 1
    ? el('select', {
        class: 'search-input',
        onchange: (e) => { location.hash = '#/dashboard?site_id=' + e.target.value; },
      }, sites.map(s => el('option', { value: String(s.id), selected: String(s.id) === siteId ? '' : null }, s.name)))
    : null;

  const head = el('div', { class: 'page-head' },
    el('div', {},
      el('h1', {}, '工地作战台'),
      el('div', { class: 'sub' }, `${d.site.name}（${d.site.code}） · 总包：${d.site.gc_company}`),
    ),
    el('div', { class: 'actions' }, siteSelector, dataTimestamp(dashRes.cachedAt, dashRes.stale)),
  );

  // ---- 统计卡 ----
  const stats = el('div', { class: 'stat-row' },
    el('div', { class: 'stat-card c-onsite' }, el('div', { class: 'num' }, funnel.onsite || 0), el('div', { class: 'label' }, '当前在场')),
    el('div', { class: 'stat-card c-pending' }, el('div', { class: 'num' }, funnel.pending || 0), el('div', { class: 'label' }, '待入场')),
    el('div', { class: 'stat-card c-leave' }, el('div', { class: 'num' }, funnel.leave || 0), el('div', { class: 'label' }, '请假离场')),
    el('div', { class: 'stat-card c-hazard' }, el('div', { class: 'num' }, d.open_hazards || 0), el('div', { class: 'label' }, '未闭环隐患')),
    el('div', { class: 'stat-card c-alert' }, el('div', { class: 'num' }, d.unread_alerts || 0), el('div', { class: 'label' }, '未读告警')),
  );

  // ---- 在场漏斗 ----
  const maxCnt = Math.max(1, ...FUNNEL_STAGES.map(([k]) => funnel[k] || 0));
  const funnelCard = el('div', { class: 'card' },
    el('h3', {}, `人员在场漏斗（建档 ${funnel.registered || 0} 人）`),
    el('div', { class: 'funnel' },
      FUNNEL_STAGES.map(([k, label]) => {
        const n = funnel[k] || 0;
        return el('div', { class: 'funnel-row' },
          el('div', { class: 'f-label' }, label),
          el('div', { class: 'f-bar-wrap' },
            el('div', { class: `f-bar st-${k}`, style: `width:${Math.round(n / maxCnt * 100)}%` })),
          el('div', { class: 'f-num' }, n),
        );
      }),
    ),
  );

  // ---- 本月出勤率（口径与出勤甘特/导出一致） ----
  const att = d.attendance || {};
  let attCaliber = localStorage.getItem('gt_caliber') || 'days';
  const attCardBox = el('div', { class: 'span-8' });
  function renderAttCard() {
    attCardBox.innerHTML = '';
    const cal = CALIBERS[attCaliber] || CALIBERS.days;
    const main = attCaliber === 'days' ? att.rate_days_pct : att.rate_hours_pct;
    const other = attCaliber === 'days' ? att.rate_hours_pct : att.rate_days_pct;
    const otherCal = attCaliber === 'days' ? CALIBERS.hours : CALIBERS.days;
    attCardBox.append(el('div', { class: 'card' },
      el('h3', {}, '本月出勤率（' + att.month + '）',
        el('span', { class: 'spacer' }),
        el('div', { class: 'cal-mini-tabs' },
          ['days', 'hours'].map(k => el('button', {
            class: 'seg-item sm' + (k === attCaliber ? ' active' : ''),
            onclick: () => { attCaliber = k; localStorage.setItem('gt_caliber', k); renderAttCard(); },
          }, CALIBERS[k].label))),
      el('div', { class: 'att-cal-head' },
        el('div', { class: 'att-cal-big' }, fmtPct(main), el('span', { class: 'dim' }, cal.name)),
        el('div', { class: 'att-cal-other' }, '另一口径 ' + fmtPct(other) + ' · ' + otherCal.label),
      ),
      el('div', { class: 'caliber-formula' }, '📐 ' + cal.formula),
      el('div', { class: 'att-mini-counts' },
        el('span', {}, '出勤 ' + (att.present_days || 0) + ' 天'),
        el('span', {}, '应出勤 ' + (att.scheduled_days || 0) + ' 天'),
        el('span', { class: 'warn-text' }, '迟 ' + (att.late_days || 0)),
        el('span', { class: 'warn-text' }, '早 ' + (att.early_days || 0)),
        el('span', { class: 'danger-text' }, '缺 ' + (att.absent_days || 0)),
        el('span', {}, '假 ' + (att.leave_days || 0)),
        el('span', { class: 'danger-text' }, '废 ' + (att.voided_days || 0)),
        el('span', {}, '工日 ' + (att.workdays || 0)),
      ),
      (att.no_punch_people > 0 || att.all_void_people > 0)
        ? el('div', { class: 'att-empty-row' },
            att.no_punch_people > 0 ? el('div', { class: 'att-empty compact nopunch' },
              el('div', { class: 'ae-icon' }, '🚫'),
              el('div', { class: 'ae-body' }, el('div', { class: 'ae-title' }, att.no_punch_people + ' 人本月无任何打卡'))) : null,
            att.all_void_people > 0 ? el('div', { class: 'att-empty compact allvoid' },
              el('div', { class: 'ae-icon' }, '🛑'),
              el('div', { class: 'ae-body' }, el('div', { class: 'ae-title' }, att.all_void_people + ' 人打卡已全部作废'))) : null)
        : null,
      (att.dirty_payroll > 0)
        ? el('div', { class: 'dirty-warn' }, `⚠️ ${att.dirty_payroll} 个已结算月存在打卡差异待复核，已发工资未受影响。`,
            el('a', { href: '#/payroll' }, ' 去分账复核 ›'))
        : null,
      el('div', { style: 'margin-top:10px' },
        el('a', { class: 'btn sm', href: '#/attendance' }, '打开出勤甘特 ›'),
        ' ',
        el('a', { class: 'btn sm ghost', href: '#/payroll' }, '工资分账 ›')),
    ));
  }
  renderAttCard();

  // ---- 今日应培训 ----
  const trainings = d.today_trainings || [];
  const trainingCard = el('div', { class: 'card' },
    el('h3', {}, `今日应培训`, el('span', { class: 'spacer' }), el('span', { class: 'tag' }, `${trainings.length} 人待训`)),
    trainings.length
      ? el('div', { class: 'list' }, trainings.map(t =>
          el('div', { class: 'list-item' },
            el('div', { class: 'grow' },
              el('div', { class: 'title' }, t.title),
              el('div', { class: 'meta' }, `${t.worker_label} · ${t.team}`),
            ),
            el('span', { class: 'tag lv-warning' }, t.status === 'pending' ? '待培训' : t.status),
          )))
      : el('div', { class: 'empty' }, '今日无待培训人员 ✓'),
  );

  // ---- 隐患 ----
  const hazards = d.hazards || [];
  const hazardCard = el('div', { class: 'card' },
    el('h3', {}, '隐患整改', el('span', { class: 'spacer' }), el('span', { class: 'tag lv-critical' }, `${hazards.length} 项未闭环`)),
    hazards.length
      ? el('div', { class: 'list' }, hazards.map(h =>
          el('div', { class: 'list-item' },
            el('div', { class: 'grow' },
              el('div', { class: 'title' }, h.title),
              el('div', { class: 'meta' }, `${fmtAgo(h.created_at)}上报`),
            ),
            el('span', { class: `tag lv-${h.level}` }, h.level),
            el('span', { class: 'tag' }, h.status === 'open' ? '待整改' : h.status === 'rectifying' ? '整改中' : h.status),
          )))
      : el('div', { class: 'empty' }, '暂无未闭环隐患 ✓'),
  );

  // ---- 告警（红点） ----
  const alerts = d.alerts || [];
  const readAllBtn = el('button', {
    class: 'btn sm ghost',
    onclick: async () => {
      try {
        await api.post('/api/alerts/read-all?site_id=' + siteId);
        toast('已全部标记已读');
        renderDashboard(root, params);
      } catch (e) { toast(e.message, 'err'); }
    },
  }, '全部已读');

  const alertCard = el('div', { class: 'card' },
    el('h3', {},
      '实时告警',
      (d.unread_alerts || 0) > 0 ? el('span', { class: 'red-dot' }, d.unread_alerts) : null,
      el('span', { class: 'spacer' }),
      (d.unread_alerts || 0) > 0 ? readAllBtn : null,
    ),
    alerts.length
      ? el('div', { class: 'list' }, alerts.map(a =>
          el('div', {
            class: 'list-item', style: 'cursor:pointer',
            onclick: async () => {
              if (a.read) return;
              try { await api.post(`/api/alerts/${a.id}/read`); a.read = true; renderDashboard(root, params); } catch (e) { toast(e.message, 'err'); }
            },
          },
            el('span', {}, a.read ? '·' : '🔴'),
            el('div', { class: 'grow' },
              el('div', { class: 'title' }, a.message),
              el('div', { class: 'meta' }, `${ALERT_TYPE_LABELS[a.type] || a.type} · ${fmtTime(a.created_at)}`),
            ),
            el('span', { class: `tag lv-${a.level}` }, a.level === 'critical' ? '紧急' : '一般'),
          )))
      : el('div', { class: 'empty' }, '暂无告警 ✓'),
  );

  root.innerHTML = '';
  root.append(
    head,
    stats,
    el('div', { class: 'dash-grid' },
      el('div', { class: 'span-8' }, funnelCard),
      el('div', { class: 'span-4' }, trainingCard),
      attCardBox,
      el('div', { class: 'span-4' }, hazardCard),
      el('div', { class: 'span-12' }, alertCard),
    ),
  );
}
