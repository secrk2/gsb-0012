package main

import (
	"database/sql"
	"fmt"
	"time"
)

// ========== 出勤打卡与分账：演示数据 ==========
//
// 时间锚定演示环境“今天”=2026-09-17：
//   - 打卡覆盖 2026-06-01 ~ 09-16（周日休息），机械班为半天班（双口径会拉开差距）
//   - 含双闸机、手机+闸机同日多源合并样本
//   - 合同 v1(6月)→v2(8/9月)→v3(9月)；6/7/8 月已结算冻结，6/7 发清，8 月部分发放
//   - 8 月结算后作废了张伟 08-25 全天打卡 → 冻结金额不变、打差异待复核
//   - 三种空态：GT-1005 无任何打卡；马超 09-14 当周全废；离线无缓存=加载失败

type seedPunch struct {
	workerID int64
	date     string
	t        string // HH:MM:SS
	dir      string
	source   string
	device   string
	status   string
	reason   string
}

func seedAttendanceBase(tx *sql.Tx, siteIDs, workerIDs, userIDs map[string]int64) error {
	s1, s2, s3 := siteIDs["GT-001"], siteIDs["GT-002"], siteIDs["GT-003"]
	gc1, gc2 := userIDs["gc_admin"], userIDs["gc_admin2"]
	teamA, teamB, teamC, teamD := "宏宇劳务·钢筋一班", "宏宇劳务·木工二班", "中建劳务·架子班", "安捷租赁·机械班"

	// ---------- 班组排班：机械班半天班（4h，午收），其余全班 ----------
	for _, sid := range []int64{s1, s2, s3} {
		for _, tm := range []struct {
			team, typ, halfEnd string
		}{
			{teamA, "full", "12:00"}, {teamB, "full", "12:00"}, {teamC, "full", "12:00"},
			{teamD, "half", "12:00"},
		} {
			if _, err := tx.Exec(`
				INSERT INTO team_schedules(site_id, team, day_type, work_start, work_end, half_end,
				                           late_grace, early_grace, full_hours, half_hours, lunch_hours)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				sid, tm.team, tm.typ, "07:30", "17:30", tm.halfEnd, 15, 15, 8, 4, 2); err != nil {
				return err
			}
		}
	}

	// ---------- 补状态史：6月有打卡的人 5 月下旬已在场 ----------
	earlyWorkers := []string{
		"GT-1001", "GT-1002", "GT-1003", "GT-1004", "GT-1006", "GT-1008", "GT-1009", "GT-1010",
		"GT-2001", "GT-2002", "GT-2004", "GT-2005", "GT-2006",
		"GT-3001", "GT-3002", "GT-3004", "GT-3005",
	}
	for _, job := range earlyWorkers {
		wid := workerIDs[job]
		tx.Exec(`INSERT INTO status_events(worker_id, from_status, to_status, actor_name, actor_role, reason, created_at)
			VALUES(?,?,?,?,?,?,?)`, wid, "none", "pending", "王建国", RoleGCAdmin, "建档登记（历史导入）", "2026-05-24T01:00:00Z")
		tx.Exec(`INSERT INTO status_events(worker_id, from_status, to_status, actor_name, actor_role, reason, created_at)
			VALUES(?,?,?,?,?,?,?)`, wid, "pending", "onsite", "王建国", RoleGCAdmin, "实名制入场（历史导入）", "2026-05-25T01:00:00Z")
	}

	// ---------- 打卡生成 ----------
	first := time.Date(2026, 6, 1, 0, 0, 0, 0, cst)
	last := time.Date(2026, 9, 16, 0, 0, 0, 0, cst)
	workingDays := []time.Time{}
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		if d.Weekday() != time.Sunday {
			workingDays = append(workingDays, d)
		}
	}

	// 每人生成规则
	type cfg struct {
		job       string
		until     string // 有打卡的最后一天（退场/拉黑/请假）
		half      bool
		persona   string // normal / late / shortday
		skipDates map[string]bool
	}
	configs := []cfg{
		{job: "GT-1001", until: "2026-09-16", persona: "normal"},
		{job: "GT-1002", until: "2026-09-16", persona: "late", skipDates: map[string]bool{"2026-09-07": true, "2026-09-08": true}},
		{job: "GT-1003", until: "2026-09-16", persona: "late"},
		{job: "GT-1004", until: "2026-09-15", persona: "normal"},
		{job: "GT-1006", until: "2026-09-16", half: true, persona: "shortday"},
		{job: "GT-1008", until: "2026-09-12", persona: "normal"},
		{job: "GT-1009", until: "2026-09-14", half: true, persona: "normal"},
		{job: "GT-1010", until: "2026-09-14", persona: "normal"},
		{job: "GT-2001", until: "2026-09-16", persona: "normal"},
		{job: "GT-2002", until: "2026-09-16", persona: "late"},
		{job: "GT-2004", until: "2026-09-16", persona: "normal"},
		{job: "GT-2005", until: "2026-09-10", persona: "normal"},
		{job: "GT-2006", until: "2026-09-16", persona: "normal"},
		{job: "GT-3001", until: "2026-09-16", persona: "normal"},
		{job: "GT-3002", until: "2026-09-13", persona: "normal"},
		{job: "GT-3004", until: "2026-09-16", persona: "late"},
		{job: "GT-3005", until: "2026-09-16", persona: "normal"},
	}

	all := []seedPunch{}
	dayIndex := map[string]int{}
	for i, d := range workingDays {
		dayIndex[d.Format("2006-01-02")] = i
	}
	for _, c := range configs {
		wid := workerIDs[c.job]
		for _, d := range workingDays {
			ds := d.Format("2006-01-02")
			if ds > c.until || c.skipDates[ds] {
				continue
			}
			idx := dayIndex[ds]
			inMin, outMin := 22, 42 // 07:22 上班、17:42 下班基线
			switch c.persona {
			case "late":
				if idx%3 == 0 {
					inMin = 61 // 08:01，迟到（>15分钟宽限）
				} else if idx%2 == 0 {
					inMin = 28
				}
				if idx%5 == 0 {
					outMin = 2 // 17:02 早退
				}
			case "shortday":
				// 半天班：多数日子做满4h，部分日子只做2.5h → 天口径仍算出勤、工时口径被拉低
				if idx%3 == 0 {
					outMin = -150 // 11:30-150min? 用单独分支处理
				}
			}
			add := func(hhmm string, dir, source, device, status, reason string) {
				t, perr := time.Parse("15:04:05", hhmm)
				if perr != nil {
					return
				}
				ts := time.Date(d.Year(), d.Month(), d.Day(), t.Hour(), t.Minute(), t.Second(), 0, cst)
				all = append(all, seedPunch{wid, ds, ts.UTC().Format(time.RFC3339), dir, source, device, status, reason})
			}
			if c.half {
				inH := 7*60 + 32
				outH := 12*60 + 5 // 12:05 收工 ≈ 4.05h（封顶4）
				if c.persona == "shortday" && idx%3 == 0 {
					outH = 10*60 + 2 // 10:02 收，约2.5h
				}
				mk := func(total int) (int, int) { return total / 60, total % 60 }
				ih, im := mk(inH)
				oh, om := mk(outH)
				add(fmtHMS(ih, im, 5+idx%7), "in", "gate", "GATE-A", "valid", "")
				add(fmtHMS(oh, om, 20+idx%5), "out", "gate", "GATE-A", "valid", "")
			} else {
				ih := 7
				if inMin >= 60 {
					ih = 8
					inMin -= 60
				}
				oh, om := 17, outMin
				add(fmtHMS(ih, inMin, 5+idx%9), "in", "gate", "GATE-A", "valid", "")
				add(fmtHMS(oh, om, 15+idx%8), "out", "gate", "GATE-A", "valid", "")
			}
		}
	}

	// 双闸机 / 手机+闸机 同日多端样本（张伟）
	addFixed := func(wid int64, date, hhmm, dir, source, device string) {
		d := parseDate(date)
		t, _ := time.Parse("15:04:05", hhmm)
		ts := time.Date(d.Year(), d.Month(), d.Day(), t.Hour(), t.Minute(), t.Second(), 0, cst)
		all = append(all, seedPunch{wid, date, ts.UTC().Format(time.RFC3339), dir, source, device, "valid", ""})
	}
	zw := workerIDs["GT-1001"]
	// 2026-07-15：两台闸机几乎同时刷到上班卡，下班闸机+手机各一笔 → 当日4笔，合并1条
	addFixed(zw, "2026-07-15", "07:20:10", "in", "gate", "GATE-A")
	addFixed(zw, "2026-07-15", "07:21:03", "in", "gate", "GATE-B")
	addFixed(zw, "2026-07-15", "17:36:40", "out", "gate", "GATE-B")
	addFixed(zw, "2026-07-15", "17:37:02", "out", "mobile", "MOBILE")
	// 2026-08-20：手机+闸机
	addFixed(zw, "2026-08-20", "07:41:55", "in", "mobile", "MOBILE")
	addFixed(zw, "2026-08-20", "18:02:11", "out", "mobile", "MOBILE")

	// 马超：09-14/09-15/09-16 三天卡全部作废（本周视图=全作废空态）
	for _, ds := range []string{"2026-09-14", "2026-09-15", "2026-09-16"} {
		// 找到并标记；直接改 all 中该人当天记录
		for i := range all {
			if all[i].workerID == workerIDs["GT-2006"] && all[i].date == ds {
				all[i].status = "void"
				all[i].reason = "疑似代打卡，监控核实后作废"
			}
		}
	}

	// ---------- 落库 ----------
	siteOf := map[int64]int64{}
	for _, c := range configs {
		switch c.job[:5] {
		case "GT-10":
			siteOf[workerIDs[c.job]] = s1
		case "GT-20":
			siteOf[workerIDs[c.job]] = s2
		case "GT-30":
			siteOf[workerIDs[c.job]] = s3
		}
	}
	actorID := map[int64]int64{s1: gc1, s2: gc2, s3: userIDs["regulator"]}
	actorName := map[int64]string{s1: "王建国", s2: "刘志远", s3: "赵正"}
	for _, p := range all {
		var lat, lng, acc any
		if p.source == "mobile" {
			lat, lng, acc = 30.2741, 120.1551, 18.0
		}
		sid := siteOf[p.workerID]
		aid, aname := actorID[sid], actorName[sid]
		if p.status == "void" {
			if _, err := tx.Exec(`
				INSERT INTO attendance_punches(site_id, worker_id, punch_date, punch_time, direction, source, device_id,
				                               lat, lng, accuracy, status, voided_by, voided_by_name, voided_reason, voided_at, created_by, created_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				sid, p.workerID, p.date, p.t, p.dir, p.source, p.source+":"+p.device,
				lat, lng, acc, "void", aid, aname, p.reason, "2026-09-16T09:00:00Z",
				aid, "2026-09-16T09:00:00Z"); err != nil {
				return err
			}
			tx.Exec(`INSERT INTO attendance_change_logs(site_id, worker_id, punch_date, month, action, month_settled, reason, actor_id, actor_name, actor_role, created_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
				sid, p.workerID, p.date, p.date[:7], "void", 0, p.reason, aid, aname, RoleGCAdmin, "2026-09-16T09:00:00Z")
		} else {
			if _, err := tx.Exec(`
				INSERT INTO attendance_punches(site_id, worker_id, punch_date, punch_time, direction, source, device_id,
				                               lat, lng, accuracy, created_by, created_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
				sid, p.workerID, p.date, p.t, p.dir, p.source, p.source+":"+p.device,
				lat, lng, acc, aid, "2026-06-01T00:00:00Z"); err != nil {
				return err
			}
		}
	}

	// ---------- 合同版本（日单价，元/工日） ----------
	baseRate := map[string]float64{
		"钢筋工": 300, "木工": 290, "架子工": 320, "电工": 300,
		"塔吊司机": 380, "信号工": 260, "普工": 240,
	}
	tradeOf := map[string]string{}
	for _, c := range configs {
		// 工种从 workers 表读不到（tx 中），按已知映射
		switch c.job {
		case "GT-1001", "GT-1002", "GT-2001", "GT-2006", "GT-3001":
			tradeOf[c.job] = "钢筋工"
		case "GT-1003", "GT-2002":
			tradeOf[c.job] = "木工"
		case "GT-1004", "GT-2004", "GT-3002":
			tradeOf[c.job] = "架子工"
		case "GT-1006", "GT-2005":
			tradeOf[c.job] = "塔吊司机"
		case "GT-1009", "GT-3004":
			tradeOf[c.job] = "信号工"
		case "GT-3005":
			tradeOf[c.job] = "电工"
		case "GT-1008":
			tradeOf[c.job] = "架子工"
		case "GT-1010":
			tradeOf[c.job] = "普工"
		}
	}
	type contractSeed struct {
		job  string
		v    int
		rate float64
		from string
		note string
	}
	contracts := []contractSeed{}
	for _, c := range configs {
		contracts = append(contracts, contractSeed{c.job, 1, baseRate[tradeOf[c.job]], "2026-06", "入场定价"})
	}
	contracts = append(contracts,
		contractSeed{"GT-1001", 2, 330, "2026-08", "夏季高温补贴后调价"},
		contractSeed{"GT-1002", 2, 330, "2026-08", "夏季高温补贴后调价"},
		contractSeed{"GT-1003", 2, 320, "2026-09", "技能津贴调价"},
		contractSeed{"GT-2001", 2, 330, "2026-09", "班组调价"},
		contractSeed{"GT-1001", 3, 360, "2026-09", "带班班长岗位津贴"},
	)
	for _, ct := range contracts {
		wid := workerIDs[ct.job]
		uid := gc1
		if ct.job[:5] == "GT-20" {
			uid = gc2
		}
		if _, err := tx.Exec(`
			INSERT INTO worker_contracts(worker_id, version, day_rate, effective_from, note, created_by, created_by_name, created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			wid, ct.v, ct.rate, ct.from, ct.note, uid, "王建国", "2026-0"+string(rune('5'+ct.v))+"-20T03:00:00Z"); err != nil {
			return err
		}
	}
	return nil
}

func fmtHMS(h, m, s int) string {
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s%60)
}

// seedAttendancePayroll 基础数据提交后执行：用同一判定引擎生成 6/7/8 月冻结快照与发放流水
func seedAttendancePayroll(db *sql.DB) error {
	svc := &server{db: db}
	siteUsers := map[int64]int64{1: 1, 2: 2, 3: 1}
	siteNames := map[int64]string{1: "王建国", 2: "刘志远", 3: "王建国"}
	for _, siteID := range []int64{1, 2, 3} {
		for _, month := range []string{"2026-06", "2026-07", "2026-08"} {
			workers, aerr := svc.scopeWorkers(&User{ID: siteUsers[siteID], Role: RoleRegulator}, siteID, "")
			if aerr != nil {
				continue
			}
			res, err := db.Exec(`INSERT INTO payroll_months(site_id, month, status, settled_at, settled_by, settled_by_name, created_at)
				VALUES(?,?, 'settled',?,?,?,?)`,
				siteID, month, month+"-30T16:00:00Z", siteUsers[siteID], siteNames[siteID], month+"-30T16:00:00Z")
			if err != nil {
				return err
			}
			pmID, _ := res.LastInsertId()
			for i := range workers {
				wk := &workers[i]
				cur := svc.liveMonthSummary(wk, month)
				if cur.Workdays <= 0 {
					continue
				}
				if _, err := db.Exec(`
					INSERT INTO payroll_items(payroll_month_id, worker_id, rate_version, day_rate, work_days, work_hours, amount, current_amount)
					VALUES(?,?,?,?,?,?,?,?)`,
					pmID, wk.ID, cur.RateVer, cur.DayRate, cur.Workdays, cur.Hours, cur.Amount, cur.Amount); err != nil {
					return err
				}
			}
		}
	}

	// 发放：6/7 月发清；8 月张伟只发 3000（部分发放），其余发清
	rows, _ := db.Query(`SELECT id, site_id, month FROM payroll_months`)
	type pm struct {
		id, site int64
		month    string
	}
	pms := []pm{}
	if rows != nil {
		for rows.Next() {
			var p pm
			if rows.Scan(&p.id, &p.site, &p.month) == nil {
				pms = append(pms, p)
			}
		}
		rows.Close()
	}
	for _, p := range pms {
		items, _ := db.Query(`SELECT worker_id, amount FROM payroll_items WHERE payroll_month_id=?`, p.id)
		type it struct {
			wid    int64
			amount float64
		}
		list := []it{}
		if items != nil {
			for items.Next() {
				var x it
				if items.Scan(&x.wid, &x.amount) == nil {
					list = append(list, x)
				}
			}
			items.Close()
		}
		for _, x := range list {
			pay := x.amount
			note := p.month + "月工资已发清"
			if p.month == "2026-08" && x.wid == 1 {
				pay = 3000
				note = "8月先发3000元，余额待发"
			}
			if _, err := db.Exec(`
				INSERT INTO payroll_payments(payroll_month_id, worker_id, amount, note, paid_by, paid_by_name, created_at)
				VALUES(?,?,?,?,?,?,?)`, p.id, x.wid, pay, note, p.site, siteNames[p.site], p.month+"-31T10:00:00Z"); err != nil {
				return err
			}
		}
	}

	// 8 月结算后，张伟 08-25 全天打卡作废 → 冻结金额不变、按最新口径算差异并打脏标记
	zw := int64(1)
	var pm8 int64
	if err := db.QueryRow(`SELECT id FROM payroll_months WHERE site_id=1 AND month='2026-08'`).Scan(&pm8); err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE attendance_punches SET status='void', voided_by=1, voided_by_name='王建国',
		voided_reason='闸机记录与刷脸复核不一致，确认代打卡，作废当日两笔打卡', voided_at='2026-09-05T03:00:00Z'
		WHERE worker_id=1 AND punch_date='2026-08-25' AND status='valid'`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		db.Exec(`INSERT INTO attendance_change_logs(site_id, worker_id, punch_date, month, action, month_settled, reason, actor_id, actor_name, actor_role, created_at)
			VALUES(1,1,'2026-08-25','2026-08','void',1,'闸机记录与刷脸复核不一致，确认代打卡，作废当日两笔打卡',1,'王建国',?, '2026-09-05T03:00:00Z')`, RoleGCAdmin)
		wk, _ := svc.getWorker(zw)
		cur := svc.liveMonthSummary(wk, "2026-08")
		db.Exec(`UPDATE payroll_items SET dirty=1, current_amount=? WHERE payroll_month_id=? AND worker_id=?`, cur.Amount, pm8, zw)
	}
	return nil
}
