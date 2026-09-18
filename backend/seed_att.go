package main

import (
	"database/sql"
	"fmt"
	"time"
)

// ========== 出勤/分账演示数据：排班规则、多源打卡、合同版本、冻结分账 ==========

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.Local)
}

func monthAdd(t time.Time, n int) time.Time {
	return t.AddDate(0, n, 0)
}

// 稳定伪随机：同样的(工号,日期)永远得到同样的异常类型
func dayHash(jobNo, day string) int {
	h := 0
	for _, c := range jobNo + day {
		h = (h*31 + int(c)) % 97
	}
	return h
}

func seedAttendance(tx *sql.Tx, siteIDs map[string]int64, workerIDs map[string]int64, adminID int64) error {
	s1 := siteIDs["GT-001"]
	s2 := siteIDs["GT-002"]
	teamA := "宏宇劳务·钢筋一班"
	teamB := "宏宇劳务·木工二班"
	teamC := "中建劳务·架子班"
	teamD := "安捷租赁·机械班"
	now := nowUTC()

	// ---------- 排班规则 ----------
	rules := []struct {
		site                      int64
		team, in, out, shift, dow string
		hours                     float64
		half                      int
	}{
		{s1, "*", "08:00", "17:30", "默认白班", "1,2,3,4,5", 8, 0},
		{s1, teamA, "08:00", "17:30", "钢筋白班", "1,2,3,4,5", 8, 0},
		{s1, teamB, "08:00", "12:00", "半天班", "1,3,5", 4, 1},
		{s1, teamC, "07:30", "17:00", "架子白班", "1,2,3,4,5", 8.5, 0},
		{s1, teamD, "07:00", "19:00", "机械两班倒", "1,2,3,4,5", 10, 0},
		{s2, "*", "08:00", "17:30", "默认白班", "1,2,3,4,5", 8, 0},
		{siteIDs["GT-003"], "*", "08:00", "17:30", "默认白班", "1,2,3,4,5", 8, 0},
	}
	for _, r := range rules {
		if _, err := tx.Exec(`
			INSERT INTO work_rules(site_id, team, shift_name, clock_in_deadline, clock_out_deadline,
			                       standard_hours, half_day, work_dow, created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`,
			r.site, r.team, r.shift, r.in, r.out, r.hours, r.half, r.dow, now); err != nil {
			return err
		}
	}

	// ---------- 合同版本（新版本只影响生效日之后） ----------
	monthA := monthStart(monthAdd(time.Now(), -2)).Format("2006-01") // 上两月：已发清并冻结
	monthB := monthStart(monthAdd(time.Now(), -1)).Format("2006-01") // 上一月：部分已发并冻结
	curMonthStart := monthStart(time.Now())
	contracts := []struct {
		team             string
		oldRate, newRate float64
		oldFrom          string
	}{
		{teamA, 280, 320, "2026-01-01"},
		{teamB, 300, 340, "2026-01-01"},
		{teamC, 260, 290, "2026-01-01"},
		{teamD, 350, 380, "2026-01-01"},
	}
	for _, c := range contracts {
		if _, err := tx.Exec(`
			INSERT INTO contract_versions(site_id, team, daily_rate, half_rate_fraction, valid_from, note, created_by, created_at)
			VALUES(?,?,?,0.5,?,?,?,?)`,
			s1, c.team, c.oldRate, c.oldFrom, "年初合同单价", "王建国", now); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO contract_versions(site_id, team, daily_rate, half_rate_fraction, valid_from, note, created_by, created_at)
			VALUES(?,?,?,0.5,?,?,?,?)`,
			s1, c.team, c.newRate, curMonthStart.Format("2006-01-02"), "三季度调差：新单价仅对生效日后出勤", "王建国", now); err != nil {
			return err
		}
	}

	// ---------- 打卡生成 ----------
	rangeStart := monthStart(monthAdd(time.Now(), -2))
	today, _ := parseDay(todayLocal())

	addPunch := func(site int64, wid int64, day, tm, source, device string) error {
		_, err := tx.Exec(`
			INSERT INTO attendance_punches(site_id, worker_id, punch_date, punch_time, source, device, created_at)
			VALUES(?,?,?,?,?,?,?)`, site, wid, day, tm, source, device, now)
		return err
	}

	// 站点1的在册工号及其当月是否排打卡（待入场/当月已退场/黑名单不排当月）
	genWorkers := []struct {
		jobNo          string
		activeCurrent  bool // 当月是否继续打卡
		halfTeam       bool
		mobileOnlySome bool
	}{
		{"GT-1001", true, false, false},
		{"GT-1002", true, false, true},   // 部分天只有手机定位卡
		{"GT-1003", true, true, false},   // 木工二班（半天班）
		{"GT-1004", false, false, false}, // 请假离场：当月不打卡
		{"GT-1006", true, false, false},
		{"GT-1009", false, false, false}, // 请假
		{"GT-1008", false, false, false}, // 已退场
		{"GT-1010", false, false, false}, // 黑名单
	}
	touched := map[int64]map[string]bool{}
	markTouched := func(wid int64, day string) {
		if touched[wid] == nil {
			touched[wid] = map[string]bool{}
		}
		touched[wid][day] = true
	}

	ruleOf := func(team string) (in, out string, half bool) {
		for _, r := range rules {
			if r.site == s1 && r.team == team {
				return r.in, r.out, r.half == 1
			}
		}
		return "08:00", "17:30", false
	}

	for _, gw := range genWorkers {
		wid := workerIDs[gw.jobNo]
		var team string
		_ = tx.QueryRow(`SELECT team FROM workers WHERE id=?`, wid).Scan(&team)
		inDead, outDead, half := ruleOf(team)
		for d := rangeStart; !d.After(today); d = d.AddDate(0, 0, 1) {
			day := d.Format("2006-01-02")
			isCurrentMonth := day[:7] == today.Format("2006-01")
			if isCurrentMonth && !gw.activeCurrent {
				continue
			}
			// 半天班只排一三五
			if half {
				if dow := int(d.Weekday()); dow != 1 && dow != 3 && dow != 5 {
					continue
				}
			} else if d.Weekday() == 0 || d.Weekday() == 6 {
				continue
			}
			h := dayHash(gw.jobNo, day)
			// 8% 缺勤（半天班提高到 6%）
			absentPct := 8
			if half {
				absentPct = 6
			}
			if h < absentPct {
				continue
			}
			inMin := hhmmToMin(inDead) - 2 - (h % 5)
			outMin := hhmmToMin(outDead) + 10 + (h%4)*7
			inTime := fmt.Sprintf("%02d:%02d:00", inMin/60, inMin%60)
			outTime := fmt.Sprintf("%02d:%02d:00", outMin/60, outMin%60)
			if h >= absentPct && h < absentPct+12 { // 约 12% 迟到
				lateMin := 8*60 + 12 + (h % 35)
				if half {
					lateMin = 8*60 + 5 + (h % 20)
				}
				inTime = fmt.Sprintf("%02d:%02d:00", lateMin/60, lateMin%60)
			}
			if h >= 70 && h < 80 { // 约 10% 早退
				var earlyMin int
				switch {
				case half:
					earlyMin = 10*60 + 20 + (h % 35) // 半天班提前下班：工时口径明显偏低
				case outMin >= 17*60:
					earlyMin = 16*60 + 10 + (h % 40)
				default:
					earlyMin = 15*60 + 10 + (h % 40)
				}
				outTime = fmt.Sprintf("%02d:%02d:00", earlyMin/60, earlyMin%60)
			}

			// 多源合并演示：GT-1001 每月 5、15 日两台闸机 + 手机定位同打
			if gw.jobNo == "GT-1001" && (d.Day() == 5 || d.Day() == 15) {
				if err := addPunch(s1, wid, day, inTime, "gate", "东门闸机A"); err != nil {
					return err
				}
				if err := addPunch(s1, wid, day, inTime, "gate", "西门闸机B"); err != nil {
					return err
				}
				if err := addPunch(s1, wid, day, inTime, "mobile", "工人手机定位"); err != nil {
					return err
				}
				if err := addPunch(s1, wid, day, outTime, "gate", "东门闸机A"); err != nil {
					return err
				}
				markTouched(wid, day)
				continue
			}
			// GT-1002 部分天只有手机定位（手机+闸机混合）
			if gw.mobileOnlySome && h%3 == 0 {
				if err := addPunch(s1, wid, day, inTime, "mobile", "工人手机定位"); err != nil {
					return err
				}
				if err := addPunch(s1, wid, day, outTime, "mobile", "工人手机定位"); err != nil {
					return err
				}
			} else {
				if err := addPunch(s1, wid, day, inTime, "gate", "东门闸机A"); err != nil {
					return err
				}
				if err := addPunch(s1, wid, day, outTime, "gate", "东门闸机A"); err != nil {
					return err
				}
				if gw.mobileOnlySome && h%5 == 0 { // 再叠加一条手机，演示手机+闸机
					if err := addPunch(s1, wid, day, inTime, "mobile", "工人手机定位"); err != nil {
						return err
					}
				}
			}
			markTouched(wid, day)
		}
	}

	// 站点2少量当月打卡（监管员切工地时有数据）
	for _, day := range []string{today.Format("2006-01-02"), today.AddDate(0, 0, -1).Format("2006-01-02")} {
		if t, _ := parseDay(day); t.Weekday() != 0 && t.Weekday() != 6 {
			if err := addPunch(s2, workerIDs["GT-2001"], day, "07:55:00", "gate", "滨江闸机"); err != nil {
				return err
			}
			if err := addPunch(s2, workerIDs["GT-2001"], day, "17:40:00", "gate", "滨江闸机"); err != nil {
				return err
			}
			markTouched(workerIDs["GT-2001"], day)
		}
	}

	// 合并日结论
	srv := &server{}
	for wid, days := range touched {
		for day := range days {
			if err := recomputeDay(tx, wid, day); err != nil {
				return err
			}
		}
	}

	// GT-1003 某天两条打卡全部作废 → 「出勤打卡全作废」空态演示
	voidDay := today.AddDate(0, 0, -3)
	for voidDay.Weekday() == 0 || voidDay.Weekday() == 6 || int(voidDay.Weekday()) == 2 || int(voidDay.Weekday()) == 4 {
		voidDay = voidDay.AddDate(0, 0, -1)
	}
	vd := voidDay.Format("2006-01-02")
	if _, err := tx.Exec(`UPDATE attendance_punches SET voided=1 WHERE worker_id=? AND punch_date=?`,
		workerIDs["GT-1003"], vd); err != nil {
		return err
	}
	if err := recomputeDay(tx, workerIDs["GT-1003"], vd); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO attendance_corrections(worker_id, day, action, from_status, to_status, reason, actor_id, actor_name, created_at)
		VALUES(?,?, 'void_punch', '出勤', '已作废', '代打卡嫌疑，当日记录全部作废待核', ?, '王建国', ?)`,
		workerIDs["GT-1003"], vd, adminID, now); err != nil {
		return err
	}

	// GT-1001 某天人工改判（补登加班出勤）
	adjDay := today.AddDate(0, 0, -2)
	if adjDay.Weekday() == 0 || adjDay.Weekday() == 6 {
		adjDay = adjDay.AddDate(0, 0, -1)
	}
	ad := adjDay.Format("2006-01-02")
	if _, err := tx.Exec(`
		INSERT INTO attendance_days(worker_id, day, status, manual, locked, updated_at)
		VALUES(?,?, 'present', 1, 0, ?)
		ON CONFLICT(worker_id, day) DO UPDATE SET status='present', manual=1, updated_at=excluded.updated_at`,
		workerIDs["GT-1001"], ad, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO attendance_corrections(worker_id, day, action, from_status, to_status, reason, actor_id, actor_name, created_at)
		VALUES(?,?, 'adjudicate', '缺勤', '出勤', '夜间抢险加班，线下核准补登', ?, '王建国', ?)`,
		workerIDs["GT-1001"], ad, adminID, now); err != nil {
		return err
	}

	// ---------- 结算冻结两个月 ----------
	if err := seedSettleMonth(srv, tx, s1, monthA, adminID, true); err != nil {
		return err
	}
	if err := seedSettleMonth(srv, tx, s1, monthB, adminID, false); err != nil {
		return err
	}
	return nil
}

func hhmm(s string) (int, int) {
	t := hhmmToMin(s)
	return t / 60, t % 60
}

// seedSettleMonth 复刻结算：快照工日×版本单价 → 冻结 → monthA 发清，monthB 部分发
func seedSettleMonth(srv *server, tx *sql.Tx, siteID int64, month string, adminID int64, payAll bool) error {
	previews, grand, aerr := srv.buildPreviewTx(tx, &payrollScope{siteID: siteID}, month)
	if aerr != nil {
		return fmt.Errorf("settle %s preview: %s", month, aerr.Msg)
	}
	res, err := tx.Exec(`
		INSERT INTO payroll_months(site_id, month, status, total_amount, paid_amount, frozen_note, settled_at, settled_by, created_at)
		VALUES(?,?,'settled',?,0,?,?, '王建国', ?)`,
		siteID, month, grand, "月末结算冻结（演示数据）", nowUTC(), nowUTC())
	if err != nil {
		return err
	}
	monthID, _ := res.LastInsertId()
	from, to, _ := monthBounds(month)
	type lineRow struct {
		wid                    int64
		team                   string
		workDays, rate, amount float64
		verFrom                string
	}
	var lineRows []lineRow
	for _, p := range previews {
		if _, err := tx.Exec(`
			INSERT INTO payroll_lines(month_id, worker_id, team, work_days, daily_rate, rate_version_date, amount, paid_amount, status)
			VALUES(?,?,?,?,?,?,?,0,'unpaid')`,
			monthID, p.WorkerID, p.Team, p.WorkDays, p.DailyRate, p.RateValidFrom, p.Amount); err != nil {
			return err
		}
		lineRows = append(lineRows, lineRow{p.WorkerID, p.Team, p.WorkDays, p.DailyRate, p.Amount, p.RateValidFrom})
	}
	if _, err := tx.Exec(`UPDATE attendance_days SET locked=1 WHERE day BETWEEN ? AND ?
		AND worker_id IN (SELECT id FROM workers WHERE site_id=?)`,
		from.Format("2006-01-02"), to.Format("2006-01-02"), siteID); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO attendance_corrections(worker_id, day, action, from_status, to_status, reason, actor_id, actor_name, created_at)
		SELECT id, ?, 'settle_lock', '', '', ?, ?, '王建国', ? FROM workers WHERE site_id=?`,
		month+"-01", "分账月 "+month+" 结算冻结", adminID, nowUTC(), siteID); err != nil {
		return err
	}

	// 发薪流水
	paidGrand := 0.0
	for i, l := range lineRows {
		pay := 0.0
		switch {
		case payAll:
			pay = l.amount
		case i == 0:
			pay = l.amount // 第一个人发清
		default:
			pay = round2(l.amount * 0.6) // 其余先发六成
		}
		if pay <= 0 {
			continue
		}
		if _, err := tx.Exec(`
			INSERT INTO payroll_payments(month_id, worker_id, amount, note, actor_name, created_at)
			VALUES(?,?,?,?,'王建国',?)`, monthID, l.wid, pay, map[bool]string{true: "工资全额发放", false: "月度工资先发六成"}[payAll], nowUTC()); err != nil {
			return err
		}
		lstatus := "partial"
		if pay >= l.amount-0.001 {
			lstatus = "paid"
		}
		if _, err := tx.Exec(`UPDATE payroll_lines SET paid_amount=?, status=? WHERE month_id=? AND worker_id=?`,
			pay, lstatus, monthID, l.wid); err != nil {
			return err
		}
		paidGrand += pay
	}
	mstatus := "partial"
	if payAll {
		mstatus = "paid"
	}
	if _, err := tx.Exec(`UPDATE payroll_months SET paid_amount=?, status=? WHERE id=?`,
		round2(paidGrand), mstatus, monthID); err != nil {
		return err
	}
	// 部分发月：给第一个人登记一笔补差（演示冻结后差额单列，不冲已发）
	if !payAll && len(lineRows) > 0 {
		if _, err := tx.Exec(`
			INSERT INTO payroll_adjustments(month_id, worker_id, amount, reason, actor_name, created_at)
			VALUES(?,?,120,'高温补贴补差（冻结后单列，不重算原快照）','王建国',?)`,
			monthID, lineRows[0].wid, nowUTC()); err != nil {
			return err
		}
	}
	return nil
}
