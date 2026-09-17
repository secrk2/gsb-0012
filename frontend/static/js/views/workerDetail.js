// ========== 人员档案详情：状态机流转 + 看全名留痕 ==========
import { api, cachedGet, submitTransition, getUser, isOnline } from '../api.js';
import { el, toast, openModal, statusChip, fmtTime, dataTimestamp, errorState, skeletonCard, STATUS_LABELS } from '../ui.js';

// (from,to) → 动作文案
function actionMeta(from, to) {
  if (to === 'onsite' && from === 'pending') return { label: '实名制入场', kind: 'primary', needVerify: true };
  if (to === 'onsite' && from === 'leave') return { label: '销假返岗', kind: 'primary' };
  if (to === 'leave') return { label: '办理请假', kind: '' };
  if (to === 'exited') return { label: '办理退场', kind: '' };
  if (to === 'blacklisted') return { label: '列入黑名单', kind: 'danger' };
  return { label: '变更为' + (STATUS_LABELS[to] || to), kind: '' };
}

export async function renderWorkerDetail(root, idOrMe) {
  root.innerHTML = '';
  root.append(skeletonCard(320));

  const path = idOrMe === 'me' ? '/api/workers/me' : '/api/workers/' + idOrMe;
  let res;
  try {
    res = await cachedGet('worker:' + idOrMe, path);
  } catch (e) {
    root.innerHTML = '';
    if (e.status === 403) {
      root.append(errorState({
        icon: '⛔', title: '无权查看该档案',
        desc: (e.message || '越权访问已被拦截') + '。如需访问请联系总包管理员授权。',
        backHash: getUser().role === 'worker' ? '#/workers/me' : '#/workers',
        backLabel: getUser().role === 'worker' ? '查看我的档案' : '返回人员列表',
      }));
    } else if (e.status === 404) {
      root.append(errorState({ icon: '🗂️', title: '档案不存在', desc: '该人员档案不存在或已被删除。', backHash: '#/workers' }));
    } else {
      root.append(errorState({ icon: '📡', title: '档案加载失败', desc: e.offline ? '当前离线且没有缓存数据，请联网后重试。' : e.message }));
    }
    return;
  }

  const w = res.data.worker;
  const me = getUser();

  // ---------- 档案卡 ----------
  const nameBox = el('span', {}, w.name);
  const revealBtn = w.can_reveal
    ? el('button', { class: 'btn sm ghost', onclick: () => openRevealModal(w.id, nameBox, revealBtn) }, '查看全名')
    : null;

  const profileCard = el('div', { class: 'card profile-card' },
    el('div', { class: 'p-head' },
      el('div', { class: 'avatar' }, (w.name || '*')[0]),
      el('div', {},
        el('div', { class: 'p-name' }, nameBox, revealBtn),
        el('div', { class: 'p-sub' }, `工号 ${w.job_no}`),
      ),
    ),
    el('div', { style: 'margin-bottom:8px' }, statusChip(w.status)),
    el('div', { class: 'verify-flags' },
      el('span', { class: 'verify-flag' + (w.face_verified ? ' ok' : '') }, w.face_verified ? '✓ 刷脸已核验' : '✗ 刷脸未核验'),
      el('span', { class: 'verify-flag' + (w.id_verified ? ' ok' : '') }, w.id_verified ? '✓ 身份证已核验' : '✗ 身份证未核验'),
    ),
    el('dl', { class: 'kv' },
      el('dt', {}, '班组'), el('dd', {}, w.team || '-'),
      el('dt', {}, '工种'), el('dd', {}, w.trade || '-'),
      el('dt', {}, '身份证'), el('dd', {}, w.id_card || '-'),
      el('dt', {}, '手机号'), el('dd', {}, w.phone || '-'),
      el('dt', {}, '建档时间'), el('dd', {}, fmtTime(w.created_at)),
      el('dt', {}, '最近变更'), el('dd', {}, fmtTime(w.updated_at)),
    ),
    w.name_masked
      ? el('div', { class: 'masked-note' }, '🔒 姓名已脱敏。总包管理员或监理可二次确认并填写理由后查看全名，查看行为将留痕。')
      : null,
  );

  // ---------- 状态流转操作 ----------
  const actions = (w.allowed_transitions || []).map(to => {
    const meta = actionMeta(w.status, to);
    return el('button', {
      class: `btn ${meta.kind}`,
      onclick: () => openTransitionModal(w, to, meta, () => renderWorkerDetail(root, idOrMe)),
    }, meta.label);
  });
  const actionCard = actions.length
    ? el('div', { class: 'card', style: 'margin-top:12px' },
        el('h3', {}, '状态变更'),
        el('div', { style: 'display:flex;gap:8px;flex-wrap:wrap' }, actions),
        !isOnline() ? el('div', { class: 'masked-note' }, '📡 当前离线：变更将进入队列，网络恢复后自动幂等同步。') : null,
      )
    : null;

  // ---------- 状态时间线 ----------
  const events = w.events || [];
  const timelineCard = el('div', { class: 'card' },
    el('h3', {}, '状态流转记录'),
    events.length
      ? el('div', { class: 'timeline' }, events.map(e =>
          el('div', { class: `tl-item tl-${e.to_status}` },
            el('div', { class: 'tl-title' }, `${e.from_label} → ${e.to_label}`),
            el('div', { class: 'tl-meta' }, `${fmtTime(e.created_at)} · 操作人：${e.actor_name}（${e.actor_role_label || e.actor_role}）`),
            e.reason ? el('div', { class: 'tl-reason' }, e.reason) : null,
          )))
      : el('div', { class: 'empty' }, '暂无流转记录'),
  );

  // ---------- 查看全名留痕 ----------
  const logs = w.reveal_logs || [];
  const revealCard = (w.reveal_logs !== undefined)
    ? el('div', { class: 'card', style: 'margin-top:16px' },
        el('h3', {}, '全名查看留痕'),
        logs.length
          ? el('div', { class: 'list' }, logs.map(l =>
              el('div', { class: 'list-item' },
                el('div', { class: 'grow' },
                  el('div', { class: 'title' }, `${l.viewer_name}（${l.viewer_role_label || l.viewer_role}）`),
                  el('div', { class: 'meta' }, `理由：${l.reason}`),
                ),
                el('span', { class: 'data-ts' }, fmtTime(l.created_at)),
              )))
          : el('div', { class: 'empty' }, '暂无查看记录'),
      )
    : null;

  // ---------- 页头 ----------
  const head = el('div', { class: 'page-head' },
    el('div', {},
      el('h1', {}, '人员档案'),
      el('div', { class: 'sub' }, me.role === 'worker' ? '本人档案' : '档案详情'),
    ),
    el('div', { class: 'actions' },
      dataTimestamp(res.cachedAt, res.stale),
      me.role !== 'worker' ? el('a', { class: 'btn ghost sm', href: '#/workers' }, '‹ 返回列表') : null,
    ),
  );

  root.innerHTML = '';
  root.append(
    head,
    el('div', { class: 'detail-grid' },
      el('div', {}, profileCard, actionCard),
      el('div', {}, timelineCard, revealCard),
    ),
  );
}

// ---------- 状态流转弹窗 ----------
function openTransitionModal(w, to, meta, onDone) {
  const reasonInput = el('textarea', { rows: '3', placeholder: to === 'blacklisted' ? '请填写拉黑原因（必填，将记入档案）' : '请填写事由（如：家中有事请假3天）' });
  const faceChk = el('input', { type: 'checkbox' });
  const idChk = el('input', { type: 'checkbox' });
  const errBox = el('div', { class: 'login-error' });

  const body = el('div', {},
    meta.needVerify
      ? el('div', {},
          el('div', { class: 'warn-box' }, '实名制入场须完成两项现场核验，勾选即代表已核验通过：'),
          el('label', { class: 'checkbox-row' }, faceChk, '刷脸核验通过（人证一致）'),
          el('label', { class: 'checkbox-row' }, idChk, '身份证核验通过（证件有效）'),
        )
      : null,
    el('div', { class: 'field' }, el('label', {}, '事由'), reasonInput),
    errBox,
  );

  openModal({
    title: `${meta.label} · ${w.name}（${w.job_no}）`,
    sub: `${STATUS_LABELS[w.status]} → ${STATUS_LABELS[to]}${isOnline() ? '' : '（当前离线，将排队同步）'}`,
    body,
    actions: [
      { label: '取消', kind: 'ghost' },
      {
        label: '确认' + meta.label,
        kind: meta.kind || 'primary',
        onClick: async (close) => {
          errBox.textContent = '';
          if (meta.needVerify && (!faceChk.checked || !idChk.checked)) {
            errBox.textContent = '请先完成刷脸与身份证核验并勾选确认';
            return;
          }
          const reason = reasonInput.value.trim();
          if ((to === 'blacklisted' || to === 'exited') && !reason) {
            errBox.textContent = '请填写事由';
            return;
          }
          try {
            const r = await submitTransition(w.id, {
              to, reason,
              face_verified: faceChk.checked,
              id_verified: idChk.checked,
            });
            close();
            if (r.queued) {
              toast('已离线排队，网络恢复后自动同步（幂等，不会重复记录）', 'warn', 3200);
            } else {
              toast(`已${meta.label}：${STATUS_LABELS[to]}`);
            }
            onDone();
          } catch (e) {
            // 状态机拦截/冲突：展示服务端给出的原因
            errBox.textContent = e.message;
            if (e.status === 409) setTimeout(onDone, 800);
          }
        },
      },
    ],
  });
}

// ---------- 查看全名弹窗（二次确认 + 理由 + 留痕） ----------
function openRevealModal(workerId, nameBox, revealBtn) {
  const reasonInput = el('input', { type: 'text', placeholder: '如：工伤认定需核对身份证信息' });
  const errBox = el('div', { class: 'login-error' });
  openModal({
    title: '查看完整姓名',
    sub: '该操作敏感，需填写查看理由，系统将留痕（谁、何时、为何查看）。',
    body: el('div', {},
      el('div', { class: 'warn-box' }, '⚠️ 查看全名仅用于业务需要，查看记录全员可追溯，请勿外传。'),
      el('div', { class: 'field' }, el('label', {}, '查看理由（必填）'), reasonInput),
      errBox,
    ),
    actions: [
      { label: '取消', kind: 'ghost' },
      {
        label: '确认查看',
        kind: 'primary',
        onClick: async (close) => {
          errBox.textContent = '';
          const reason = reasonInput.value.trim();
          if (reason.length < 2) { errBox.textContent = '请填写查看理由（不少于2个字）'; return; }
          try {
            const r = await api.post(`/api/workers/${workerId}/reveal-name`, { reason });
            close();
            nameBox.textContent = r.full_name;
            revealBtn.remove();
            toast(r.notice || '本次查看已留痕', 'warn', 3200);
          } catch (e) {
            errBox.textContent = e.message;
          }
        },
      },
    ],
  });
}
