// ========== 出勤率双口径：文案三处（作战台 / 出勤打卡详情 / 导出）必须逐字一致 ==========

export const CALIBER_DAYS_TEXT = '出勤率（按出勤天）= 实际出勤天 ÷ 应出勤天';
export const CALIBER_HOURS_TEXT = '出勤率（按打卡工时）= 有效打卡工时 ÷ 排班工时';

export const CALIBERS = {
  days: {
    key: 'days',
    label: '按出勤天',
    text: CALIBER_DAYS_TEXT,
    pick: (t) => t.rate_days,
  },
  hours: {
    key: 'hours',
    label: '按打卡工时',
    text: CALIBER_HOURS_TEXT,
    pick: (t) => t.rate_hours,
  },
};

const STORE_KEY = 'gt_caliber';

export function getCaliber() {
  const v = localStorage.getItem(STORE_KEY);
  return v === 'hours' ? 'hours' : 'days'; // 默认按出勤天
}

export function setCaliber(k) {
  localStorage.setItem(STORE_KEY, k === 'hours' ? 'hours' : 'days');
}

export function pct(v) {
  const n = Number(v) || 0;
  return (n * 100).toFixed(1) + '%';
}

export function hoursOf(min) {
  return ((Number(min) || 0) / 60).toFixed(1);
}

// 半天班班组两口径结论可能相反的固定提示
export const HALF_DAY_NOTE = '半天班班组：按出勤天按 0.5 工日计应出勤；按打卡工时按实际首入—末出计。两口径数值可能不一致，均为正式口径，以本界面所选为准。';
