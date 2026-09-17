// ========== 出勤率口径：作战台 / 出勤甘特详情 / 导出 三处共用同一文案 ==========
// 后端 /api/attendance/gantt 也会下发 calibers 定义；本文件保证离线/无数据时文案仍一致。

export const CALIBERS = {
  days: {
    key: 'days',
    label: '天口径',
    name: '实际出勤天 / 应出勤天',
    formula: '出勤率 = 实际出勤天数 ÷ 应出勤天数（当天有有效打卡即计为出勤1天，迟到/早退仍计出勤）',
    note: '「人到了就算」：半天班、只打半天卡的人当天仍计1个出勤天，出勤率偏高。',
  },
  hours: {
    key: 'hours',
    label: '工时口径',
    name: '有效打卡工时 / 排班工时',
    formula: '出勤率 = 有效打卡工时 ÷ 排班工时（半天班排班工时按4小时/天，首末班打卡跨度扣午休）',
    note: '「干了多久算多少」：迟到、早退、半天班会拉低出勤率。半天班多的班组，两口径结果可能相反。',
  },
};

export function caliber(key) {
  return CALIBERS[key] || CALIBERS.days;
}

export function fmtPct(v) {
  const n = Number(v || 0);
  return (Math.round(n * 10) / 10).toFixed(1) + '%';
}

export function fmtMoney(v) {
  return '¥' + (Number(v || 0)).toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
}
