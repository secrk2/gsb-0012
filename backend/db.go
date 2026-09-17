package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

func openDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite 单写者：限制连接数避免 database is locked
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		return nil, err
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS sites (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  code TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  address TEXT NOT NULL DEFAULT '',
  gc_company TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active'
);

CREATE TABLE IF NOT EXISTS workers (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  job_no TEXT NOT NULL UNIQUE,
  full_name TEXT NOT NULL,
  id_card TEXT NOT NULL DEFAULT '',
  phone TEXT NOT NULL DEFAULT '',
  team TEXT NOT NULL DEFAULT '',
  trade TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  face_verified INTEGER NOT NULL DEFAULT 0,
  id_verified INTEGER NOT NULL DEFAULT 0,
  version INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_workers_site_status ON workers(site_id, status);

CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,
  role TEXT NOT NULL,
  name TEXT NOT NULL,
  site_id INTEGER REFERENCES sites(id),
  team TEXT NOT NULL DEFAULT '',
  worker_id INTEGER REFERENCES workers(id)
);

CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL REFERENCES users(id),
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS status_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  from_status TEXT NOT NULL,
  to_status TEXT NOT NULL,
  actor_id INTEGER,
  actor_name TEXT NOT NULL DEFAULT '',
  actor_role TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  client_op_id TEXT,
  created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_status_events_client_op
  ON status_events(client_op_id) WHERE client_op_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_status_events_worker ON status_events(worker_id);

-- 离线变更幂等表：client_op_id 唯一，重放直接返回首次结果，不产生重复记录
CREATE TABLE IF NOT EXISTS client_ops (
  client_op_id TEXT PRIMARY KEY,
  actor_id INTEGER NOT NULL,
  worker_id INTEGER NOT NULL,
  to_status TEXT NOT NULL,
  result TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS reveal_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  viewer_id INTEGER NOT NULL,
  viewer_name TEXT NOT NULL,
  viewer_role TEXT NOT NULL,
  reason TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_reveal_logs_worker ON reveal_logs(worker_id);

CREATE TABLE IF NOT EXISTS trainings (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  title TEXT NOT NULL,
  due_date TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
);
CREATE INDEX IF NOT EXISTS idx_trainings_site_due ON trainings(site_id, due_date);

CREATE TABLE IF NOT EXISTS hazards (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  title TEXT NOT NULL,
  level TEXT NOT NULL DEFAULT '一般',
  status TEXT NOT NULL DEFAULT 'open',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS alerts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  type TEXT NOT NULL,
  message TEXT NOT NULL,
  level TEXT NOT NULL DEFAULT 'warning',
  read INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_alerts_site ON alerts(site_id, read);

-- ========== 出勤打卡与分账 ==========

-- 班组排班（一个工地一个班组一条；班制 full/half 决定工日与排班工时）
CREATE TABLE IF NOT EXISTS team_schedules (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  team TEXT NOT NULL,
  day_type TEXT NOT NULL DEFAULT 'full',        -- full=全班 / half=半天班
  work_start TEXT NOT NULL DEFAULT '07:30',
  work_end TEXT NOT NULL DEFAULT '17:30',
  half_end TEXT NOT NULL DEFAULT '12:00',
  late_grace INTEGER NOT NULL DEFAULT 15,       -- 迟到宽限（分钟）
  early_grace INTEGER NOT NULL DEFAULT 15,      -- 早退宽限（分钟）
  full_hours REAL NOT NULL DEFAULT 8,           -- 全班标准工时（已扣午休）
  half_hours REAL NOT NULL DEFAULT 4,           -- 半天标准工时
  lunch_hours REAL NOT NULL DEFAULT 2,          -- 全班打卡跨度扣减的午休
  UNIQUE(site_id, team)
);

-- 原始打卡流水：闸机/手机上报的每一笔都留底；日级记录由查询时聚合（同人同天多源合并为一条）
CREATE TABLE IF NOT EXISTS attendance_punches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  punch_date TEXT NOT NULL,                     -- 服务端按东八区算出的本地日期
  punch_time TEXT NOT NULL,                     -- RFC3339
  direction TEXT NOT NULL,                      -- in / out
  source TEXT NOT NULL,                         -- gate / mobile
  device_id TEXT NOT NULL DEFAULT '',
  lat REAL, lng REAL, accuracy REAL,
  status TEXT NOT NULL DEFAULT 'valid',         -- valid / void
  voided_by INTEGER, voided_by_name TEXT NOT NULL DEFAULT '',
  voided_reason TEXT NOT NULL DEFAULT '',
  voided_at TEXT,
  client_op_id TEXT,
  created_by INTEGER, created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_punches_worker_date ON attendance_punches(worker_id, punch_date);
CREATE INDEX IF NOT EXISTS idx_punches_site_date ON attendance_punches(site_id, punch_date);
CREATE UNIQUE INDEX IF NOT EXISTS ux_punches_client_op
  ON attendance_punches(client_op_id) WHERE client_op_id IS NOT NULL;

-- 打卡/作废/恢复/补卡的操作留痕（动已结算月必须带原因）
CREATE TABLE IF NOT EXISTS attendance_change_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL,
  worker_id INTEGER NOT NULL,
  punch_id INTEGER,
  punch_date TEXT NOT NULL DEFAULT '',
  month TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,                         -- punch / void / restore
  month_settled INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL DEFAULT '',
  actor_id INTEGER, actor_name TEXT NOT NULL DEFAULT '', actor_role TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attlog_worker ON attendance_change_logs(worker_id, punch_date);

-- 日单价合同版本：新版本只影响生效月及以后，历史结算月在 payroll_items 中冻结单价
CREATE TABLE IF NOT EXISTS worker_contracts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  version INTEGER NOT NULL,
  day_rate REAL NOT NULL,                       -- 元/工日
  effective_from TEXT NOT NULL,                 -- YYYY-MM
  note TEXT NOT NULL DEFAULT '',
  created_by INTEGER, created_by_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(worker_id, version)
);
CREATE INDEX IF NOT EXISTS idx_contracts_worker ON worker_contracts(worker_id, effective_from);

-- 月分账单（冻结后不可重算）
CREATE TABLE IF NOT EXISTS payroll_months (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  month TEXT NOT NULL,                          -- YYYY-MM
  status TEXT NOT NULL DEFAULT 'open',          -- open / settled
  settled_at TEXT, settled_by INTEGER, settled_by_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(site_id, month)
);

-- 月分账人员行：结算瞬间的快照（工日、单价版本、单价、金额全部冻结）
CREATE TABLE IF NOT EXISTS payroll_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  payroll_month_id INTEGER NOT NULL REFERENCES payroll_months(id),
  worker_id INTEGER NOT NULL,
  rate_version INTEGER NOT NULL,
  day_rate REAL NOT NULL,
  work_days REAL NOT NULL,
  work_hours REAL NOT NULL DEFAULT 0,
  amount REAL NOT NULL,
  dirty INTEGER NOT NULL DEFAULT 0,             -- 结算后打卡被改动 → 1，等待复核
  current_amount REAL NOT NULL DEFAULT 0,       -- 按最新打卡实时重算的金额（仅供复核对照）
  UNIQUE(payroll_month_id, worker_id)
);
CREATE INDEX IF NOT EXISTS idx_payitems_month ON payroll_items(payroll_month_id);

-- 已发工资：独立流水，任何重算/复核都不得修改或冲销
CREATE TABLE IF NOT EXISTS payroll_payments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  payroll_month_id INTEGER NOT NULL REFERENCES payroll_months(id),
  worker_id INTEGER NOT NULL,
  amount REAL NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  paid_by INTEGER, paid_by_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payments_month_worker ON payroll_payments(payroll_month_id, worker_id);

-- 复核调整项：结算后打卡差异经二次确认入账（正负补差），delta=0 表示复核后维持原金额
CREATE TABLE IF NOT EXISTS payroll_adjustments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  payroll_month_id INTEGER NOT NULL REFERENCES payroll_months(id),
  worker_id INTEGER NOT NULL,
  delta REAL NOT NULL,
  reason TEXT NOT NULL,
  actor_id INTEGER, actor_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payadj_month_worker ON payroll_adjustments(payroll_month_id, worker_id);
`
	_, err := db.Exec(schema)
	return err
}
