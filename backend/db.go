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

-- 排班规则：按工地+班组配置（无匹配班组时用 team='*' 的默认规则）
CREATE TABLE IF NOT EXISTS work_rules (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  team TEXT NOT NULL DEFAULT '*',
  shift_name TEXT NOT NULL DEFAULT '白班',
  clock_in_deadline TEXT NOT NULL DEFAULT '08:00',   -- 晚于该时间算迟到
  clock_out_deadline TEXT NOT NULL DEFAULT '17:30',  -- 早于该时间下班算早退
  standard_hours REAL NOT NULL DEFAULT 8.0,
  half_day INTEGER NOT NULL DEFAULT 0,               -- 半天班：排班工时按 0.5 工日
  work_dow TEXT NOT NULL DEFAULT '1,2,3,4,5',        -- 应出勤星期（0=周日 … 6=周六）
  created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_work_rules ON work_rules(site_id, team);

-- 打卡流水（原始记录）。同人同天可有多条（两台闸机 / 手机+闸机），
-- attendance_days 是其合并后的唯一日结论，打卡记录不删，只作作废标记。
CREATE TABLE IF NOT EXISTS attendance_punches (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  punch_date TEXT NOT NULL,                 -- 本地日期 YYYY-MM-DD
  punch_time TEXT NOT NULL,                 -- 本地时间 HH:MM:SS
  source TEXT NOT NULL DEFAULT 'gate',      -- gate=闸机 / mobile=手机定位
  device TEXT NOT NULL DEFAULT '',          -- 闸机编号或定位来源
  client_op_id TEXT,
  voided INTEGER NOT NULL DEFAULT 0,        -- 单条作废（误打卡）
  created_at TEXT NOT NULL
);
-- 同人同天同设备同时刻只记一笔（两台闸机各打各的也允许，但同设备重放幂等）
CREATE UNIQUE INDEX IF NOT EXISTS ux_punch_op ON attendance_punches(client_op_id) WHERE client_op_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_punch_worker_date ON attendance_punches(worker_id, punch_date);

-- 日结论（同人同天唯一）：由当日有效打卡合并判定
CREATE TABLE IF NOT EXISTS attendance_days (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  day TEXT NOT NULL,                        -- YYYY-MM-DD
  status TEXT NOT NULL DEFAULT 'absent',    -- present/late/early/absent/rest/void
  first_in TEXT NOT NULL DEFAULT '',
  last_out TEXT NOT NULL DEFAULT '',
  sources TEXT NOT NULL DEFAULT '',         -- 合并来源，如 gate(2)+mobile(1)
  punch_count INTEGER NOT NULL DEFAULT 0,
  valid_punch_count INTEGER NOT NULL DEFAULT 0,
  work_minutes INTEGER NOT NULL DEFAULT 0,  -- 有效打卡工时（首入到末出）
  all_voided INTEGER NOT NULL DEFAULT 0,    -- 当日打卡全部作废
  manual INTEGER NOT NULL DEFAULT 0,        -- 人工改判锁定（不再被打卡自动重算）
  locked INTEGER NOT NULL DEFAULT 0,        -- 被已结算月锁定，修改需二次确认留痕
  updated_at TEXT NOT NULL,
  UNIQUE(worker_id, day)
);

-- 出勤修正留痕：作废/反作废/手工改判，凡动已结算月强制二次确认+原因
CREATE TABLE IF NOT EXISTS attendance_corrections (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  day TEXT NOT NULL,
  action TEXT NOT NULL,                    -- void_punch/restore_punch/adjudicate/settle_lock/unlock
  from_status TEXT NOT NULL DEFAULT '',
  to_status TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL,
  actor_id INTEGER,
  actor_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_corr_worker_day ON attendance_corrections(worker_id, day);

-- 合同日单价版本：按班组，valid_from 起生效；新版本只影响之后，历史结算月冻结原版本
CREATE TABLE IF NOT EXISTS contract_versions (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  team TEXT NOT NULL,
  daily_rate REAL NOT NULL,                -- 元/工日（出勤+迟到计全工日，早退按 half_rate_fraction）
  half_rate_fraction REAL NOT NULL DEFAULT 1.0, -- 早退/半天折算系数（0.5=半个工日）
  valid_from TEXT NOT NULL,                -- YYYY-MM-DD
  note TEXT NOT NULL DEFAULT '',
  created_by TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_contract_team ON contract_versions(site_id, team, valid_from);

-- 分账月：一旦结算，工日/单价/金额被冻结，后续出勤修正不影响本快照
CREATE TABLE IF NOT EXISTS payroll_months (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  site_id INTEGER NOT NULL REFERENCES sites(id),
  month TEXT NOT NULL,                     -- YYYY-MM
  status TEXT NOT NULL DEFAULT 'open',     -- open/settled/paid/partial
  total_amount REAL NOT NULL DEFAULT 0,    -- 快照应付总额
  paid_amount REAL NOT NULL DEFAULT 0,
  frozen_note TEXT NOT NULL DEFAULT '',    -- 结算说明（冻结时写入）
  settled_at TEXT NOT NULL DEFAULT '',
  settled_by TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  UNIQUE(site_id, month)
);

-- 分账明细：结算时按当时合同版本单价冻结
CREATE TABLE IF NOT EXISTS payroll_lines (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  month_id INTEGER NOT NULL REFERENCES payroll_months(id),
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  team TEXT NOT NULL DEFAULT '',
  work_days REAL NOT NULL DEFAULT 0,       -- 计薪工日（出勤+迟到全工日，早退折算）
  daily_rate REAL NOT NULL DEFAULT 0,      -- 冻结时的日单价
  rate_version_date TEXT NOT NULL DEFAULT '',
  amount REAL NOT NULL DEFAULT 0,          -- 工日×单价，冻结
  paid_amount REAL NOT NULL DEFAULT 0,     -- 已发：只增不减，重算绝不冲销
  status TEXT NOT NULL DEFAULT 'unpaid',   -- unpaid/partial/paid
  UNIQUE(month_id, worker_id)
);

-- 发薪流水：支持部分发放；已发金额只能由正向流水累加
CREATE TABLE IF NOT EXISTS payroll_payments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  month_id INTEGER NOT NULL REFERENCES payroll_months(id),
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  amount REAL NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  actor_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_month ON payroll_payments(month_id, worker_id);

-- 差额调整：冻结月出勤修正后的补差/扣减（有原因留痕），与发薪流水分开，不冲销已发
CREATE TABLE IF NOT EXISTS payroll_adjustments (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  month_id INTEGER NOT NULL REFERENCES payroll_months(id),
  worker_id INTEGER NOT NULL REFERENCES workers(id),
  amount REAL NOT NULL,
  reason TEXT NOT NULL,
  actor_name TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_adjust_month ON payroll_adjustments(month_id, worker_id);
`
	_, err := db.Exec(schema)
	return err
}
