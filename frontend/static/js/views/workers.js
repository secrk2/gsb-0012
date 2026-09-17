// ========== 人员档案列表 ==========
import { cachedGet, getUser } from '../api.js';
import { el, statusChip, dataTimestamp, errorState, skeletonCard, STATUS_LABELS } from '../ui.js';

const FILTERS = [
  ['', '全部'],
  ['pending', '待入场'],
  ['onsite', '在场'],
  ['leave', '请假离场'],
  ['exited', '退场'],
  ['blacklisted', '黑名单'],
];

export async function renderWorkers(root, params) {
  const user = getUser();
  const status = params.get('status') || '';
  const q = params.get('q') || '';

  const searchInput = el('input', {
    class: 'search-input', type: 'search',
    placeholder: '搜索工号 / 工种 / 班组…', value: q,
  });
  let debounce;
  searchInput.addEventListener('input', () => {
    clearTimeout(debounce);
    debounce = setTimeout(() => {
      const v = searchInput.value.trim();
      location.hash = '#/workers' + buildQuery(status, v);
    }, 350);
  });

  const head = el('div', { class: 'page-head' },
    el('div', {},
      el('h1', {}, '人员档案'),
      el('div', { class: 'sub' }, user.role === 'sub_leader' ? `数据范围：本班组（${user.team}）· 姓名已脱敏` : '姓名已脱敏展示，查看全名需二次确认并留痕'),
    ),
    el('div', { class: 'actions' }, searchInput),
  );

  const filterBar = el('div', { class: 'filter-bar' },
    FILTERS.map(([k, label]) => el('button', {
      class: 'filter-chip' + (status === k ? ' active' : ''),
      onclick: () => { location.hash = '#/workers' + buildQuery(k, q); },
    }, label)),
  );

  const listBox = el('div', {}, skeletonCard(300));
  root.innerHTML = '';
  root.append(head, filterBar, listBox);

  const qs = buildQuery(status, q).slice(1);
  let res;
  try {
    res = await cachedGet('workers:' + (qs || 'all'), '/api/workers' + (qs ? '?' + qs : ''));
  } catch (e) {
    listBox.innerHTML = '';
    if (e.status === 403) {
      listBox.append(errorState({ icon: '⛔', title: '无权查看人员列表', desc: e.message }));
    } else {
      listBox.append(errorState({ icon: '📡', title: '人员数据加载失败', desc: e.offline ? '当前离线且没有缓存数据，请联网后重试。' : e.message }));
    }
    return;
  }

  const workers = res.data.workers || [];
  listBox.innerHTML = '';
  listBox.append(
    el('div', { style: 'display:flex;justify-content:flex-end;margin-bottom:8px' },
      dataTimestamp(res.cachedAt, res.stale)),
  );
  if (!workers.length) {
    listBox.append(el('div', { class: 'empty' }, '没有符合条件的人员'));
    return;
  }
  listBox.append(el('div', { class: 'worker-grid' },
    workers.map(w => el('div', {
      class: 'worker-card',
      onclick: () => { location.hash = '#/workers/' + w.id; },
    },
      el('div', { class: 'wc-head' },
        el('div', { class: 'avatar' }, (w.name || '*')[0]),
        el('div', {},
          el('div', { class: 'wc-name' }, w.name),
          el('div', { class: 'wc-job' }, `工号 ${w.job_no}`),
        ),
      ),
      el('div', { class: 'wc-meta' },
        el('span', { class: 'tag' }, w.trade),
        el('span', { class: 'tag' }, w.team),
      ),
      el('div', { class: 'wc-foot' },
        statusChip(w.status),
        el('span', { class: 'data-ts' }, '详情 ›'),
      ),
    )),
  ));
}

function buildQuery(status, q) {
  const p = new URLSearchParams();
  if (status) p.set('status', status);
  if (q) p.set('q', q);
  const s = p.toString();
  return s ? '?' + s : '';
}
