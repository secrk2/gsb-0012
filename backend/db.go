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
`
	_, err := db.Exec(schema)
	return err
}
