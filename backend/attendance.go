package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ========== 出勤打卡与分账：判定引擎 ==========
//
// 设计要点：
//  1. attendance_punches 是原始流水（闸机/手机每一笔都留底），日级记录查询时聚合：
//     同人同天多台闸机、或手机+闸机同时打到 → 合并为一条日记录，绝不记两笔。
//  2. 每人每天判定 出勤/缺勤/迟到/早退（可迟到且早退并存），请假/未入场/已退场/已作废 单独标注。
//  3. 工日 = 有效打卡工时 / 8（半天班出勤半天=0.5工日），工资 = 工日 × 当月适用合同单价。
//  4. 已结算月份快照冻结；之后打卡变动只打脏标记，不冲销已发工资，复核经二次确认+原因后补差。

var cst = time.FixedZone("CST", 8*3600)

// 出勤结论码
const (
	AttPresent   = "present"    // 出勤
	AttAbsent    = "absent"     // 缺勤
	AttLate      = "late"       // 迟到
	AttEarly     = "early"      // 早退
	AttLateEarly = "late_early" // 迟到且早退
	AttLeave     = "leave"      // 请假
	AttVoided    = "voided"     // 打卡已作废
	AttExcluded  = "excluded"   // 未入场/已退场/非排班
)

var attLabels = map[string]string{
	AttPresent:   "出勤",
	AttAbsent:    "缺勤",
	AttLate:      "迟到",
	AttEarly:     "早退",
	AttLateEarly: "迟到·早退",
	AttLeave:     "请假",
	AttVoided:    "打卡已作废",
	AttExcluded:  "非排班",
}

// 两个出勤率口径（作战台 / 出勤详情 / 导出 三处共用同一组定义）
const (
	CaliberDays  = "days"
	CaliberHours = "hours"
)

var caliberMeta = map[string]map[string]string{
	CaliberDays: {
		"key":     CaliberDays,
		"label":   "天口径",
		"name":    "实际出勤天 / 应出勤天",
		"formula": "出勤率 = 实际出勤天数 ÷ 应出勤天数（当天有有效打卡即计为出勤1天，迟到/早退仍计出勤）",
		"note":    "「人到了就算」：半天班、只打半天卡的人当天仍计1个出勤天，出勤率偏高。",
	},
	CaliberHours: {
		"key":     CaliberHours,
		"label":   "工时口径",
		"name":    "有效打卡工时 / 排班工时",
		"formula": "出勤率 = 有效打卡工时 ÷ 排班工时（半天班排班工时按4小时/天，首末班打卡跨度扣午休）",
		"note":    "「干了多久算多少」：迟到、早退、半天班会拉低出勤率。半天班多的班组，两口径结果可能相反。",
	},
}

func normalizeCaliber(c string) string {
	if c == CaliberHours {
		return CaliberHours
	}
	return CaliberDays
}

func otherCaliber(c string) string {
	if c == CaliberDays {
		return CaliberHours
	}
	return CaliberDays
}

// ---------- 排班 ----------

type schedule struct {
	DayType    string  `json:"day_type"`
	WorkStart  string  `json:"work_start"`
	WorkEnd    string  `json:"work_end"`
	HalfEnd    string  `json:"half_end"`
	LateGrace  int     `json:"late_grace"`
	EarlyGrace int     `json:"early_grace"`
	FullHours  float64 `json:"full_hours"`
	HalfHours  float64 `json:"half_hours"`
	LunchHours float64 `json:"lunch_hours"`
}

func defaultSchedule() schedule {
	return schedule{DayType: "full", WorkStart: "07:30", WorkEnd: "17:30", HalfEnd: "12:00",
		LateGrace: 15, EarlyGrace: 15, FullHours: 8, HalfHours: 4, LunchHours: 2}
}

func (s *server) getSchedule(siteID int64, team string) schedule {
	sc := defaultSchedule()
	err := s.db.QueryRow(`
		SELECT day_type, work_start, work_end, half_end, late_grace, early_grace,
		       full_hours, half_hours, lunch_hours
		FROM team_schedules WHERE site_id = ? AND team = ?`, siteID, team).
		Scan(&sc.DayType, &sc.WorkStart, &sc.WorkEnd, &sc.HalfEnd, &sc.LateGrace, &sc.EarlyGrace,
			&sc.FullHours, &sc.HalfHours, &sc.LunchHours)
	if err == nil {
		return sc
	}
	return defaultSchedule()
}

// ---------- 日历工具 ----------

func parseDate(s string) time.Time {
	t, _ := time.ParseInLocation("2006-01-02", s, cst)
	return t
}

func dateOf(t time.Time) string { return t.In(cst).Format("2006-01-02") }

func todayInCST() string { return time.Now().In(cst).Format("2006-01-02") }

func addDays(s string, n int) string {
	return parseDate(s).AddDate(0, 0, n).Format("2006-01-02")
}

func monthRange(month string) (string, string) {
	first := parseDate(month + "-01")
	return first.Format("2006-01-02"), first.AddDate(0, 1, -1).Format("2006-01-02")
}

// weekRange 返回 date 所在周的周一~周日（date 为空取今天）
func weekRange(date string) (string, string) {
	d := time.Now().In(cst)
	if date != "" {
		d = parseDate(date)
	}
	wd := int(d.Weekday())
	if wd == 0 {
		wd = 7
	}
	monday := d.AddDate(0, 0, -(wd - 1))
	return monday.Format("2006-01-02"), monday.AddDate(0, 0, 6).Format("2006-01-02")
}

func eachDate(from, to string) []string {
	out := []string{}
	for d := parseDate(from); !d.After(parseDate(to)); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out
}

func hhmm(s string, day time.Time) time.Time {
	parts := strings.Split(s, ":")
	h, _ := strconv.Atoi(parts[0])
	m := 0
	if len(parts) > 1 {
		m, _ = strconv.Atoi(parts[1])
	}
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, cst)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func pct(num, den float64) float64 {
	if den <= 0 {
		return 0
	}
	return math.Round(num/den*1000) / 10 // 百分比保留1位小数
}

func placeholders(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ",")
}

// ---------- 状态事件 → 在/请假区间 ----------

type dateEvent struct {
	Date string
	To   string
}

type dateSpan struct{ From, To string }

type eligibility struct {
	Onsite []dateSpan // 在场计出勤区间（含退场当天）
	Leave  []dateSpan // 请假区间
	ExitAt string     // 退场/拉黑日期，空表示仍在场
}

func (e eligibility) classifyDay(d string) string {
	for _, sp := range e.Leave {
		if d >= sp.From && d <= sp.To {
			return AttLeave
		}
	}
	for _, sp := range e.Onsite {
		if d >= sp.From && d <= sp.To {
			return "onsite"
		}
	}
	if e.ExitAt != "" && d > e.ExitAt {
		return "exited"
	}
	return "not_started"
}

func buildEligibility(events []dateEvent, today string) eligibility {
	sort.Slice(events, func(i, j int) bool { return events[i].Date < events[j].Date })
	res := eligibility{}
	var onsiteStart, leaveStart string
	for _, ev := range events {
		switch ev.To {
		case string(StatusOnsite):
			if leaveStart != "" {
				res.Leave = append(res.Leave, dateSpan{leaveStart, addDays(ev.Date, -1)})
				leaveStart = ""
			}
			if onsiteStart == "" {
				onsiteStart = ev.Date
			}
		case string(StatusLeave):
			if onsiteStart != "" {
				res.Onsite = append(res.Onsite, dateSpan{onsiteStart, addDays(ev.Date, -1)})
				onsiteStart = ""
			}
			leaveStart = ev.Date
		case string(StatusExited), string(StatusBlacklisted):
			if onsiteStart != "" {
				res.Onsite = append(res.Onsite, dateSpan{onsiteStart, ev.Date}) // 退场当天仍计在场
				onsiteStart = ""
			}
			if leaveStart != "" {
				res.Leave = append(res.Leave, dateSpan{leaveStart, ev.Date})
				leaveStart = ""
			}
			res.ExitAt = ev.Date
		}
	}
	if onsiteStart != "" {
		res.Onsite = append(res.Onsite, dateSpan{onsiteStart, today})
	}
	if leaveStart != "" {
		res.Leave = append(res.Leave, dateSpan{leaveStart, today})
	}
	return res
}

// ---------- 打卡流水 ----------

type punchRow struct {
	ID           int64
	WorkerID     int64
	PunchDate    string
	PunchTime    time.Time
	PunchTimeStr string
	Direction    string
	Source       string
	DeviceID     string
	Status       string
	VoidedByName string
	VoidedReason string
	VoidedAt     string
}

func sourceLabel(s string) string {
	if s == "mobile" {
		return "手机定位"
	}
	return "闸机"
}

// ---------- 日聚合单元 ----------

type dayCell struct {
	Date        string   `json:"date"`
	Weekday     int      `json:"weekday"`
	Status      string   `json:"status"`
	StatusLabel string   `json:"status_label"`
	Kind        string   `json:"kind"` // scheduled / leave / excluded / voided
	Hours       float64  `json:"hours"`
	StdHours    float64  `json:"std_hours"`
	Workday     float64  `json:"workday"` // 工日 = 有效工时/8，封顶1
	PunchCount  int      `json:"punch_count"`
	Sources     []string `json:"sources"`
	SourceLabel []string `json:"source_labels"`
	Devices     []string `json:"devices"`
	Merged      bool     `json:"merged"`
	MissingIn   bool     `json:"missing_in"`
	MissingOut  bool     `json:"missing_out"`
	FirstIn     string   `json:"first_in"`
	LastOut     string   `json:"last_out"`
	AllVoid     bool     `json:"all_void"`
	Note        string   `json:"note"`
	PunchIDs    []int64  `json:"punch_ids"`
}

func (c *dayCell) appendSource(p punchRow) {
	seen := false
	for _, x := range c.Sources {
		if x == p.Source {
			seen = true
		}
	}
	if !seen {
		c.Sources = append(c.Sources, p.Source)
		c.SourceLabel = append(c.SourceLabel, sourceLabel(p.Source))
	}
	c.Devices = append(c.Devices, p.DeviceID)
}

// judgeCell 把一天的打卡聚合成一条判定结果（同人同天多源合并，绝不记两笔）
func judgeCell(date string, sc schedule, elig eligibility, punches []punchRow) dayCell {
	day := parseDate(date)
	cell := dayCell{Date: date, Weekday: int(day.Weekday()), Status: AttExcluded, StatusLabel: attLabels[AttExcluded],
		Kind: "excluded", Sources: []string{}, Devices: []string{}, PunchIDs: []int64{}}

	valid, voided := []punchRow{}, []punchRow{}
	for _, p := range punches {
		cell.PunchIDs = append(cell.PunchIDs, p.ID)
		if p.Status == "void" {
			voided = append(voided, p)
		} else {
			valid = append(valid, p)
		}
	}
	cell.PunchCount = len(punches)

	dayKind := elig.classifyDay(date)
	if dayKind == AttLeave && len(valid) == 0 {
		cell.Status, cell.StatusLabel, cell.Kind = AttLeave, attLabels[AttLeave], "leave"
		return cell
	}
	// 工地默认周日休息：无有效卡即非排班，不计缺勤分母（加班打了卡则正常判定）
	if day.Weekday() == time.Sunday && len(valid) == 0 {
		for _, p := range punches {
			cell.appendSource(p)
		}
		cell.Note = "休息日（周日）"
		if len(voided) > 0 {
			cell.AllVoid = true
		}
		return cell
	}
	if dayKind == "not_started" || dayKind == "exited" {
		for _, p := range voided {
			cell.appendSource(p)
		}
		if len(valid) == 0 {
			cell.Note = map[string]string{"not_started": "尚未实名制入场", "exited": "已退场/已离工地"}[dayKind]
			if len(voided) > 0 {
				cell.AllVoid = true
				cell.Status, cell.StatusLabel = AttVoided, attLabels[AttVoided]
			}
			return cell
		}
		// 入场前/退场后的打卡：标注场外，不计出勤率、不计工日
		cell.Note = "非排班日打卡（未入场或已退场），不计出勤"
	}

	stdHours := sc.FullHours
	endClock := sc.WorkEnd
	if sc.DayType == "half" {
		stdHours = sc.HalfHours
		endClock = sc.HalfEnd
	}
	cell.StdHours = stdHours

	// 有打卡流水但全部作废：在场期间按缺勤口径单列「已作废」（恢复后自动改回出勤）
	if len(valid) == 0 {
		for _, p := range voided {
			cell.appendSource(p)
		}
		if len(voided) > 0 {
			cell.AllVoid = true
			if dayKind == "onsite" {
				cell.Status, cell.StatusLabel, cell.Kind = AttVoided, attLabels[AttVoided], "voided"
				cell.Note = "当日打卡已全部作废，按缺勤处理；如系误作废可恢复"
			}
			return cell
		}
		// 一笔打卡都没有
		if date > todayInCST() {
			cell.Note = "未到排班日"
			return cell
		}
		if dayKind == "onsite" {
			cell.Status, cell.StatusLabel, cell.Kind = AttAbsent, attLabels[AttAbsent], "scheduled"
			cell.Note = "全天无打卡记录"
		}
		return cell
	}

	sort.Slice(valid, func(i, j int) bool { return valid[i].PunchTime.Before(valid[j].PunchTime) })
	var firstIn, lastOut *punchRow
	for i := range valid {
		p := valid[i]
		cell.appendSource(p)
		if p.Direction == "in" && firstIn == nil {
			fp := p
			firstIn = &fp
		}
		if p.Direction == "out" {
			lp := p
			lastOut = &lp
		}
	}
	// 合并判定：不同设备/不同来源（双闸机、手机+闸机）同一天打到 → 一条
	dedupDev := map[string]bool{}
	devs := []string{}
	for _, d := range cell.Devices {
		if !dedupDev[d] {
			dedupDev[d] = true
			devs = append(devs, d)
		}
	}
	cell.Devices = devs
	cell.Merged = len(devs) > 1 || len(cell.Sources) > 1

	late, early := false, false
	noteParts := []string{}
	workStart := hhmm(sc.WorkStart, day)
	workEnd := hhmm(endClock, day)
	spanHours := func(t1, t2 time.Time) float64 {
		mins := t2.Sub(t1).Minutes()
		if sc.DayType != "half" {
			mins -= sc.LunchHours * 60
		}
		return math.Max(0, mins/60)
	}

	switch {
	case firstIn != nil && lastOut != nil:
		cell.Hours = spanHours(firstIn.PunchTime, lastOut.PunchTime)
		cell.FirstIn = firstIn.PunchTime.In(cst).Format("15:04")
		cell.LastOut = lastOut.PunchTime.In(cst).Format("15:04")
	case firstIn != nil:
		// 缺下班卡：工时算到排班下班时刻，结论按早退（可补卡/恢复纠正）
		cell.Hours = spanHours(firstIn.PunchTime, workEnd)
		cell.FirstIn = firstIn.PunchTime.In(cst).Format("15:04")
		cell.MissingOut = true
		early = true
		noteParts = append(noteParts, "缺下班打卡，按早退处理")
	default:
		// 缺上班卡：工时从排班上班时刻算起，结论按迟到
		last := valid[len(valid)-1]
		lp := last
		lastOut = &lp
		cell.Hours = spanHours(workStart, lp.PunchTime)
		cell.LastOut = lp.PunchTime.In(cst).Format("15:04")
		cell.MissingIn = true
		late = true
		noteParts = append(noteParts, "缺上班打卡，按迟到处理")
	}
	cell.Hours = round2(math.Min(stdHours, cell.Hours))
	cell.Workday = round2(math.Min(1, cell.Hours/8))

	if firstIn != nil {
		if lateMins := workStart.Add(time.Duration(sc.LateGrace) * time.Minute).Sub(firstIn.PunchTime); lateMins < 0 {
			late = true
			noteParts = append(noteParts, fmt.Sprintf("迟到%d分钟", int(-lateMins.Minutes())+1))
		}
	}
	if lastOut != nil {
		if earlyMins := lastOut.PunchTime.Sub(workEnd.Add(-time.Duration(sc.EarlyGrace) * time.Minute)); earlyMins < 0 {
			early = true
			noteParts = append(noteParts, fmt.Sprintf("早退%d分钟", int(-earlyMins.Minutes())+1))
		}
	}

	switch {
	case late && early:
		cell.Status = AttLateEarly
	case late:
		cell.Status = AttLate
	case early:
		cell.Status = AttEarly
	default:
		cell.Status = AttPresent
	}
	cell.StatusLabel = attLabels[cell.Status]
	cell.Kind = "scheduled"
	if cell.Merged {
		noteParts = append(noteParts, "当日多端打卡（"+strings.Join(cell.SourceLabel, "+")+"）已合并为一条")
	}
	cell.Note = strings.Join(noteParts, "；")
	return cell
}

// ---------- 区间汇总 ----------

type personAttendance struct {
	WorkerID       int64              `json:"worker_id"`
	JobNo          string             `json:"job_no"`
	Name           string             `json:"name"`
	NameMasked     bool               `json:"name_masked"`
	Team           string             `json:"team"`
	Trade          string             `json:"trade"`
	Schedule       schedule           `json:"schedule"`
	DayTypeLabel   string             `json:"day_type_label"`
	Cells          map[string]dayCell `json:"cells"`
	ScheduledDays  int                `json:"scheduled_days"`
	PresentDays    int                `json:"present_days"`
	AbsentDays     int                `json:"absent_days"`
	VoidedDays     int                `json:"voided_days"`
	LeaveDays      int                `json:"leave_days"`
	LateDays       int                `json:"late_days"`
	EarlyDays      int                `json:"early_days"`
	Hours          float64            `json:"hours"`
	ScheduledHours float64            `json:"scheduled_hours"`
	Workdays       float64            `json:"workdays"`
	RateDays       float64            `json:"rate_days_pct"`
	RateHours      float64            `json:"rate_hours_pct"`
	HasAnyPunch    bool               `json:"has_any_punch"`
	AllVoid        bool               `json:"all_void"`
}

func isPresentStatus(st string) bool {
	return st == AttPresent || st == AttLate || st == AttEarly || st == AttLateEarly
}

// buildAttendance 为给定工人在区间内逐日判定
func (s *server) buildAttendance(wk *Worker, dates []string, punchesByDate map[string][]punchRow, eligCache map[int64]eligibility) *personAttendance {
	sc := s.getSchedule(wk.SiteID, wk.Team)
	elig, ok := eligCache[wk.ID]
	if !ok {
		elig = s.loadEligibility(wk.ID, dates[len(dates)-1])
		eligCache[wk.ID] = elig
	}
	pa := &personAttendance{
		WorkerID: wk.ID, JobNo: wk.JobNo, Name: maskName(wk.FullName), NameMasked: true,
		Team: wk.Team, Trade: wk.Trade, Schedule: sc, Cells: map[string]dayCell{},
		DayTypeLabel: map[string]string{"full": "全班制（8h）", "half": "半天班（4h）"}[sc.DayType],
	}
	punchDays, voidPunchDays := 0, 0
	for _, d := range dates {
		cell := judgeCell(d, sc, elig, punchesByDate[d])
		pa.Cells[d] = cell
		if len(punchesByDate[d]) > 0 {
			punchDays++
			if cell.AllVoid {
				voidPunchDays++
			}
		}
		switch cell.Kind {
		case "scheduled":
			pa.ScheduledDays++
			pa.ScheduledHours += cell.StdHours
			if isPresentStatus(cell.Status) {
				pa.PresentDays++
				pa.Hours += cell.Hours
				pa.Workdays += cell.Workday
				if cell.Status == AttLate || cell.Status == AttLateEarly {
					pa.LateDays++
				}
				if cell.Status == AttEarly || cell.Status == AttLateEarly {
					pa.EarlyDays++
				}
			} else {
				pa.AbsentDays++
			}
		case "voided":
			pa.ScheduledDays++
			pa.ScheduledHours += cell.StdHours
			pa.VoidedDays++
		case "leave":
			pa.LeaveDays++
		}
	}
	pa.HasAnyPunch = punchDays > 0
	pa.AllVoid = punchDays > 0 && voidPunchDays == punchDays
	pa.Hours, pa.Workdays = round2(pa.Hours), round2(pa.Workdays)
	pa.RateDays = pct(float64(pa.PresentDays), float64(pa.ScheduledDays))
	pa.RateHours = pct(pa.Hours, pa.ScheduledHours)
	return pa
}

func (s *server) loadEligibility(workerID int64, today string) eligibility {
	rows, err := s.db.Query(`SELECT to_status, created_at FROM status_events WHERE worker_id = ? ORDER BY created_at`, workerID)
	if err != nil {
		return eligibility{}
	}
	defer rows.Close()
	evs := []dateEvent{}
	for rows.Next() {
		var to, created string
		if rows.Scan(&to, &created) == nil {
			if t, perr := time.Parse(time.RFC3339, created); perr == nil {
				evs = append(evs, dateEvent{Date: dateOf(t), To: to})
			}
		}
	}
	return buildEligibility(evs, today)
}

// loadPunches 拉取区间内打卡并按 (worker,date) 分组
func (s *server) loadPunches(siteID int64, workerIDs []int64, from, to string) (map[int64]map[string][]punchRow, error) {
	out := map[int64]map[string][]punchRow{}
	if len(workerIDs) == 0 {
		return out, nil
	}
	q := `SELECT id, worker_id, punch_date, punch_time, direction, source, device_id, status,
	             COALESCE(voided_by_name,''), COALESCE(voided_reason,''), COALESCE(voided_at,'')
	      FROM attendance_punches
	      WHERE site_id = ? AND punch_date BETWEEN ? AND ?
	        AND worker_id IN (` + placeholders(len(workerIDs)) + `)
	      ORDER BY punch_time`
	args := []any{siteID, from, to}
	for _, id := range workerIDs {
		args = append(args, id)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p punchRow
		var pt string
		if err := rows.Scan(&p.ID, &p.WorkerID, &p.PunchDate, &pt, &p.Direction, &p.Source, &p.DeviceID,
			&p.Status, &p.VoidedByName, &p.VoidedReason, &p.VoidedAt); err != nil {
			continue
		}
		t, _ := time.Parse(time.RFC3339, pt)
		p.PunchTime = t
		p.PunchTimeStr = pt
		if out[p.WorkerID] == nil {
			out[p.WorkerID] = map[string][]punchRow{}
		}
		out[p.WorkerID][p.PunchDate] = append(out[p.WorkerID][p.PunchDate], p)
	}
	return out, nil
}

// ---------- 数据范围 ----------

func (s *server) scopeWorkers(u *User, siteID int64, teamFilter string) ([]Worker, *apiError) {
	q := `SELECT id, site_id, job_no, full_name, team, trade, status FROM workers WHERE site_id = ?`
	args := []any{siteID}
	switch u.Role {
	case RoleRegulator, RoleGCAdmin, RoleSupervisor:
	case RoleSubLeader:
		if u.SiteID == nil || *u.SiteID != siteID {
			return nil, &apiError{HTTPCode: 403, Msg: "班组长只能查看本班组出勤"}
		}
		if teamFilter != "" && teamFilter != u.Team {
			return nil, &apiError{HTTPCode: 403, Msg: "班组长只能查看本班组出勤"}
		}
		q += ` AND team = ?`
		args = append(args, u.Team)
	case RoleWorker:
		if u.WorkerID == nil {
			return nil, &apiError{HTTPCode: 403, Msg: "账号未绑定人员档案"}
		}
		q += ` AND id = ?`
		args = append(args, *u.WorkerID)
	default:
		return nil, &apiError{HTTPCode: 403, Msg: "无权查看出勤数据"}
	}
	if teamFilter != "" && u.Role != RoleSubLeader && u.Role != RoleWorker {
		q += ` AND team = ?`
		args = append(args, teamFilter)
	}
	q += ` ORDER BY team, job_no`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, &apiError{HTTPCode: 500, Msg: "查询失败"}
	}
	defer rows.Close()
	list := []Worker{}
	for rows.Next() {
		var w Worker
		if rows.Scan(&w.ID, &w.SiteID, &w.JobNo, &w.FullName, &w.Team, &w.Trade, &w.Status) == nil {
			list = append(list, w)
		}
	}
	return list, nil
}

// canManageAttendance 打卡/作废权限
func canManageAttendance(u *User, wk *Worker) bool {
	switch u.Role {
	case RoleGCAdmin, RoleSupervisor:
		return u.SiteID != nil && *u.SiteID == wk.SiteID
	case RoleSubLeader:
		return u.SiteID != nil && *u.SiteID == wk.SiteID && u.Team == wk.Team
	case RoleWorker:
		return u.WorkerID != nil && *u.WorkerID == wk.ID
	}
	return false
}

// resolveAttendanceSite 解析可见工地（监管员可切换，其余限本工地）
func (s *server) resolveAttendanceSite(u *User, q url.Values) (int64, *apiError) {
	siteID := int64(0)
	if v := q.Get("site_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, &apiError{HTTPCode: 400, Msg: "工地编号格式错误"}
		}
		siteID = id
	}
	if u.Role == RoleRegulator {
		if siteID == 0 {
			if err := s.db.QueryRow(`SELECT id FROM sites ORDER BY id LIMIT 1`).Scan(&siteID); err != nil {
				return 0, &apiError{HTTPCode: 404, Msg: "暂无工地数据"}
			}
		}
		return siteID, nil
	}
	if u.SiteID == nil {
		return 0, &apiError{HTTPCode: 403, Msg: "账号未绑定工地"}
	}
	if siteID != 0 && siteID != *u.SiteID {
		return 0, &apiError{HTTPCode: 403, Msg: "无权查看其他工地的出勤"}
	}
	return *u.SiteID, nil
}

// ---------- HTTP: 上报打卡 ----------

type punchReq struct {
	WorkerID   int64   `json:"worker_id"`
	PunchTime  string  `json:"punch_time"`
	Direction  string  `json:"direction"`
	Source     string  `json:"source"`
	DeviceID   string  `json:"device_id"`
	Lat        float64 `json:"lat"`
	Lng        float64 `json:"lng"`
	Accuracy   float64 `json:"accuracy"`
	ClientOpID string  `json:"client_op_id"`
}

func (s *server) handlePunch(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	var req punchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.Direction != "in" && req.Direction != "out" {
		writeErr(w, http.StatusBadRequest, "打卡方向必须是 in（上班）或 out（下班）")
		return
	}
	if req.Source != "gate" && req.Source != "mobile" {
		writeErr(w, http.StatusBadRequest, "打卡来源必须是 gate（闸机）或 mobile（手机定位）")
		return
	}
	if u.Role == RoleWorker {
		if u.WorkerID == nil {
			writeErr(w, http.StatusForbidden, "账号未绑定人员档案")
			return
		}
		if req.WorkerID != 0 && req.WorkerID != *u.WorkerID {
			writeErr(w, http.StatusForbidden, "手机定位打卡只能为本人打卡，不能代他人打卡")
			return
		}
		req.WorkerID = *u.WorkerID
		req.Source = "mobile"
	}
	if u.Role == RoleRegulator {
		writeErr(w, http.StatusForbidden, "监管员账号为只读，不能打卡")
		return
	}
	if req.WorkerID == 0 {
		writeErr(w, http.StatusBadRequest, "缺少打卡人员")
		return
	}
	wk, err := s.getWorker(req.WorkerID)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "人员档案不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	if !canManageAttendance(u, wk) {
		writeErr(w, http.StatusForbidden, "无权为该人员打卡")
		return
	}

	when := time.Now().In(cst)
	if req.PunchTime != "" {
		t, perr := time.Parse(time.RFC3339, req.PunchTime)
		if perr != nil {
			writeErr(w, http.StatusBadRequest, "打卡时间格式错误，需 RFC3339")
			return
		}
		when = t.In(cst)
	}
	if req.DeviceID == "" {
		req.DeviceID = map[string]string{"gate": "GATE-X", "mobile": "MOBILE"}[req.Source]
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()

	// 1) client_op_id 幂等：重放直接返回首次结果
	if req.ClientOpID != "" {
		var existing int64
		if err := tx.QueryRow(`SELECT id FROM attendance_punches WHERE client_op_id = ?`, req.ClientOpID).Scan(&existing); err == nil {
			tx.Rollback()
			writeJSON(w, http.StatusOK, map[string]any{
				"punch": map[string]any{"id": existing, "duplicate": true}, "dedup_reason": "client_op_id",
				"notice": "该打卡已上报过，已幂等去重",
			})
			return
		}
	}

	// 2) 同设备同人同方向 3 分钟内重复刷卡：只留一笔（闸机连刷/重复提交）；跨闸机、手机+闸机不去重，留到日聚合合并
	var dupID int64
	if err := tx.QueryRow(`
		SELECT id FROM attendance_punches
		WHERE worker_id = ? AND device_id = ? AND direction = ? AND status = 'valid'
		  AND ABS(strftime('%s', punch_time) - strftime('%s', ?)) < 180
		ORDER BY id DESC LIMIT 1`,
		req.WorkerID, req.Source+":"+req.DeviceID, req.Direction, when.UTC().Format(time.RFC3339)).Scan(&dupID); err == nil {
		tx.Rollback()
		writeJSON(w, http.StatusOK, map[string]any{
			"punch": map[string]any{"id": dupID, "duplicate": true}, "dedup_reason": "same_device_3min",
			"notice": "同一设备3分钟内重复打卡，已合并为一笔",
		})
		return
	}

	var opID any
	if req.ClientOpID != "" {
		opID = req.ClientOpID
	}
	var lat, lng, acc any
	if req.Source == "mobile" && (req.Lat != 0 || req.Lng != 0) {
		lat, lng, acc = req.Lat, req.Lng, req.Accuracy
	}
	res, err := tx.Exec(`
		INSERT INTO attendance_punches(site_id, worker_id, punch_date, punch_time, direction, source, device_id,
		                               lat, lng, accuracy, client_op_id, created_by, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		wk.SiteID, wk.ID, when.Format("2006-01-02"), when.UTC().Format(time.RFC3339),
		req.Direction, req.Source, req.Source+":"+req.DeviceID,
		lat, lng, acc, opID, u.ID, nowUTC())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "打卡失败，请重试")
		return
	}
	pid, _ := res.LastInsertId()
	tx.Exec(`INSERT INTO attendance_change_logs(site_id, worker_id, punch_id, punch_date, month, action, month_settled, actor_id, actor_name, actor_role, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		wk.SiteID, wk.ID, pid, when.Format("2006-01-02"), when.Format("2006-01"), "punch", 0, u.ID, u.Name, u.Role, nowUTC())
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "打卡失败，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"punch":      map[string]any{"id": pid, "duplicate": false},
		"punch_date": when.Format("2006-01-02"),
		"punch_time": when.UTC().Format(time.RFC3339),
	})
}

// ---------- HTTP: 甘特 ----------

func (s *server) handleGantt(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query()
	siteID, aerr := s.resolveAttendanceSite(u, q)
	if aerr != nil {
		aerr.write(w)
		return
	}
	view := "week"
	if q.Get("view") == "month" {
		view = "month"
	}
	var from, to string
	if view == "month" {
		m := q.Get("month")
		if m == "" {
			m = time.Now().In(cst).Format("2006-01")
		}
		from, to = monthRange(m)
	} else {
		from, to = weekRange(q.Get("date"))
	}
	caliber := normalizeCaliber(q.Get("caliber"))
	team := q.Get("team")

	workers, aerr := s.scopeWorkers(u, siteID, team)
	if aerr != nil {
		aerr.write(w)
		return
	}
	dates := eachDate(from, to)
	ids := make([]int64, 0, len(workers))
	for _, wk := range workers {
		ids = append(ids, wk.ID)
	}
	punches, err := s.loadPunches(siteID, ids, from, to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询打卡失败")
		return
	}

	people := []*personAttendance{}
	eligCache := map[int64]eligibility{}
	sum := &personAttendance{}
	teamSet := map[string]bool{}
	for i := range workers {
		wk := &workers[i]
		pa := s.buildAttendance(wk, dates, punches[wk.ID], eligCache)
		if u.Role == RoleWorker && u.WorkerID != nil && *u.WorkerID == wk.ID {
			pa.Name = wk.FullName // 本人可见全名
			pa.NameMasked = false
		}
		people = append(people, pa)
		teamSet[wk.Team] = true
		sum.ScheduledDays += pa.ScheduledDays
		sum.PresentDays += pa.PresentDays
		sum.AbsentDays += pa.AbsentDays
		sum.VoidedDays += pa.VoidedDays
		sum.LeaveDays += pa.LeaveDays
		sum.LateDays += pa.LateDays
		sum.EarlyDays += pa.EarlyDays
		sum.Hours += pa.Hours
		sum.ScheduledHours += pa.ScheduledHours
		sum.Workdays += pa.Workdays
	}
	sum.RateDays = pct(float64(sum.PresentDays), float64(sum.ScheduledDays))
	sum.RateHours = pct(sum.Hours, sum.ScheduledHours)
	sum.Hours, sum.Workdays = round2(sum.Hours), round2(sum.Workdays)

	teams := make([]string, 0, len(teamSet))
	for t := range teamSet {
		teams = append(teams, t)
	}
	sort.Strings(teams)

	writeJSON(w, http.StatusOK, map[string]any{
		"view": view, "from": from, "to": to, "dates": dates,
		"caliber":     caliberMeta[caliber],
		"calibers":    []map[string]string{caliberMeta[CaliberDays], caliberMeta[CaliberHours]},
		"team":        team,
		"teams":       teams,
		"people":      people,
		"totals":      sum,
		"server_time": nowUTC(),
	})
}

// ---------- HTTP: 单日打卡明细 ----------

func (s *server) handleDayPunches(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	wid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "人员编号格式错误")
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		writeErr(w, http.StatusBadRequest, "缺少 date 参数")
		return
	}
	wk, gerr := s.getWorker(wid)
	if gerr == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "人员档案不存在")
		return
	}
	if gerr != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	if aerr := canViewWorker(u, wk); aerr != nil {
		aerr.write(w)
		return
	}

	rows, qerr := s.db.Query(`
		SELECT id, punch_time, direction, source, device_id, lat, lng, accuracy, status,
		       COALESCE(voided_by_name,''), COALESCE(voided_reason,''), COALESCE(voided_at,'')
		FROM attendance_punches WHERE worker_id = ? AND punch_date = ? ORDER BY punch_time`, wid, date)
	if qerr != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		var id int64
		var pt, dir, src, dev, status, vbn, vr, va string
		var lat, lng, acc sql.NullFloat64
		if rows.Scan(&id, &pt, &dir, &src, &dev, &lat, &lng, &acc, &status, &vbn, &vr, &va) != nil {
			continue
		}
		list = append(list, map[string]any{
			"id": id, "punch_time": pt, "direction": dir,
			"direction_label": map[string]string{"in": "上班卡", "out": "下班卡"}[dir],
			"source":          src, "source_label": sourceLabel(src), "device_id": dev,
			"lat": lat.Float64, "lng": lng.Float64, "accuracy": acc.Float64,
			"status": status, "voided_by_name": vbn, "voided_reason": vr, "voided_at": va,
			// 工人可给自己手机打卡，但作废/恢复仅管理者
			"can_manage": canManageAttendance(u, wk),
			"can_void":   u.Role != RoleWorker && u.Role != RoleRegulator && canManageAttendance(u, wk),
		})
	}
	month := date[:7]
	var settled int
	s.db.QueryRow(`SELECT COUNT(*) FROM payroll_months WHERE site_id = ? AND month = ? AND status = 'settled'`, wk.SiteID, month).Scan(&settled)

	writeJSON(w, http.StatusOK, map[string]any{
		"worker_id": wid, "date": date, "month": month, "month_settled": settled > 0,
		"punches": list, "server_time": nowUTC(),
	})
}

// ---------- HTTP: 作废 / 恢复 ----------

type voidReq struct {
	Reason         string `json:"reason"`
	ConfirmSettled bool   `json:"confirm_settled"`
}

func (s *server) handleVoidPunch(w http.ResponseWriter, r *http.Request, restore bool) {
	u := currentUser(r)
	if u.Role == RoleWorker || u.Role == RoleRegulator {
		if restore {
			writeErr(w, http.StatusForbidden, "当前角色无权恢复打卡")
		} else {
			writeErr(w, http.StatusForbidden, "当前角色无权作废打卡")
		}
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "打卡编号格式错误")
		return
	}
	var req voidReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)

	var siteID, workerID int64
	var pdate, status string
	err = s.db.QueryRow(`SELECT site_id, worker_id, punch_date, status FROM attendance_punches WHERE id = ?`, id).
		Scan(&siteID, &workerID, &pdate, &status)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "打卡记录不存在")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	wk, gerr := s.getWorker(workerID)
	if gerr != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	if !canManageAttendance(u, wk) {
		writeErr(w, http.StatusForbidden, "无权操作该人员的打卡")
		return
	}
	if restore && status == "valid" {
		writeErr(w, http.StatusConflict, "该打卡本就有效，无需恢复")
		return
	}
	if !restore && status == "void" {
		writeErr(w, http.StatusConflict, "该打卡已作废，请勿重复操作")
		return
	}

	month := pdate[:7]
	var settled int
	s.db.QueryRow(`SELECT COUNT(*) FROM payroll_months WHERE site_id = ? AND month = ? AND status = 'settled'`, siteID, month).Scan(&settled)
	if len([]rune(req.Reason)) < 2 {
		if settled > 0 {
			writeErr(w, http.StatusBadRequest, month+" 月已结算冻结：改动该月打卡必须填写原因（不少于2个字）并二次确认，操作将留痕")
		} else {
			writeErr(w, http.StatusBadRequest, "请填写作废/恢复原因（不少于2个字）")
		}
		return
	}
	if settled > 0 && !req.ConfirmSettled {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":         month + " 月已结算冻结，修改会影响已结工资，需二次确认",
			"need_confirm":  true,
			"month_settled": true,
			"month":         month,
		})
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()
	action := "void"
	if restore {
		action = "restore"
		if _, err := tx.Exec(`UPDATE attendance_punches SET status='valid', voided_by=NULL, voided_by_name='', voided_reason='', voided_at=NULL WHERE id=?`, id); err != nil {
			writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
			return
		}
	} else {
		if _, err := tx.Exec(`UPDATE attendance_punches SET status='void', voided_by=?, voided_by_name=?, voided_reason=?, voided_at=? WHERE id=?`,
			u.ID, u.Name, req.Reason, nowUTC(), id); err != nil {
			writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
			return
		}
	}
	if _, err := tx.Exec(`INSERT INTO attendance_change_logs(site_id, worker_id, punch_id, punch_date, month, action, month_settled, reason, actor_id, actor_name, actor_role, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		siteID, workerID, id, pdate, month, action, settled, req.Reason, u.ID, u.Name, u.Role, nowUTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	var pmID int64
	if settled > 0 {
		tx.QueryRow(`SELECT id FROM payroll_months WHERE site_id = ? AND month = ? AND status='settled'`, siteID, month).Scan(&pmID)
		tx.Exec(`UPDATE payroll_items SET dirty = 1 WHERE payroll_month_id = ? AND worker_id = ?`, pmID, workerID)
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	// 提交后再按最新打卡重算对照金额（只读参考，绝不改冻结金额/已发工资）
	if settled > 0 && pmID > 0 {
		if cur := s.liveMonthSummary(wk, month); cur != nil {
			s.db.Exec(`UPDATE payroll_items SET current_amount = ? WHERE payroll_month_id = ? AND worker_id = ?`,
				cur.Amount, pmID, workerID)
		}
	}
	msg := "打卡已作废"
	if restore {
		msg = "打卡已恢复"
	}
	if settled > 0 {
		msg += "。" + month + "月已结算，原金额与已发工资保持不变，差异已标记，待总包在分账页复核补差"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg, "month_settled": settled > 0})
}

// ---------- HTTP: 出勤导出 CSV（口径与界面一致，表头写明公式） ----------

func (s *server) handleAttendanceExport(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query()
	siteID, aerr := s.resolveAttendanceSite(u, q)
	if aerr != nil {
		aerr.write(w)
		return
	}
	view := "week"
	if q.Get("view") == "month" {
		view = "month"
	}
	var from, to, periodName string
	if view == "month" {
		m := q.Get("month")
		if m == "" {
			m = time.Now().In(cst).Format("2006-01")
		}
		from, to = monthRange(m)
		periodName = m + "月"
	} else {
		from, to = weekRange(q.Get("date"))
		periodName = from + "_" + to + "周"
	}
	caliber := normalizeCaliber(q.Get("caliber"))
	workers, aerr := s.scopeWorkers(u, siteID, q.Get("team"))
	if aerr != nil {
		aerr.write(w)
		return
	}
	dates := eachDate(from, to)
	ids := make([]int64, 0, len(workers))
	for _, wk := range workers {
		ids = append(ids, wk.ID)
	}
	punches, _ := s.loadPunches(siteID, ids, from, to)
	eligCache := map[int64]eligibility{}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="attendance_%s.csv"`, periodName))
	w.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM，Excel 不乱码
	cw := csv.NewWriter(w)

	meta := caliberMeta[caliber]
	cw.Write([]string{"# 出勤导出（" + periodName + "）"})
	cw.Write([]string{"# 主口径【" + meta["label"] + "】：" + meta["formula"]})
	cw.Write([]string{"# 另一口径：" + caliberMeta[otherCaliber(caliber)]["formula"]})
	cw.Write([]string{"# 状态缩写：勤=出勤 迟=迟到 早=早退 迟早=迟到·早退 缺=缺勤 假=请假 废=打卡已作废 -=非排班"})

	header := []string{"工号", "姓名（脱敏）", "班组", "工种", "班制"}
	for _, d := range dates {
		header = append(header, d[5:])
	}
	header = append(header,
		"应出勤天", "实际出勤天", "出勤率(天口径)%",
		"排班工时", "有效打卡工时", "出勤率(工时口径)%",
		"迟到天数", "早退天数", "缺勤天数", "请假天数", "作废天数", "出勤工日(=有效工时/8)")
	cw.Write(header)

	statusShort := map[string]string{
		AttPresent: "勤", AttLate: "迟", AttEarly: "早", AttLateEarly: "迟早",
		AttAbsent: "缺", AttLeave: "假", AttVoided: "废", AttExcluded: "-",
	}
	for i := range workers {
		wk := &workers[i]
		pa := s.buildAttendance(wk, dates, punches[wk.ID], eligCache)
		row := []string{wk.JobNo, pa.Name, wk.Team, wk.Trade, pa.DayTypeLabel}
		for _, d := range dates {
			c := pa.Cells[d]
			if isPresentStatus(c.Status) {
				row = append(row, statusShort[c.Status]+"("+strconv.FormatFloat(c.Hours, 'f', 0, 64)+"h)")
			} else {
				row = append(row, statusShort[c.Status])
			}
		}
		row = append(row,
			strconv.Itoa(pa.ScheduledDays), strconv.Itoa(pa.PresentDays), strconv.FormatFloat(pa.RateDays, 'f', 1, 64),
			strconv.FormatFloat(pa.ScheduledHours, 'f', 0, 64), strconv.FormatFloat(pa.Hours, 'f', 0, 64), strconv.FormatFloat(pa.RateHours, 'f', 1, 64),
			strconv.Itoa(pa.LateDays), strconv.Itoa(pa.EarlyDays), strconv.Itoa(pa.AbsentDays), strconv.Itoa(pa.LeaveDays),
			strconv.Itoa(pa.VoidedDays), strconv.FormatFloat(pa.Workdays, 'f', 2, 64))
		cw.Write(row)
	}
	cw.Flush()
}

// siteAttendanceSummary 作战台用：当月至今双口径出勤率（与甘特/导出同一引擎）
func (s *server) siteAttendanceSummary(u *User, siteID int64, month string) map[string]any {
	from, to := monthRange(month)
	today := time.Now().In(cst).Format("2006-01-02")
	if to > today {
		to = today
	}
	if from > to {
		from = to
	}
	team := ""
	if u.Role == RoleSubLeader {
		team = u.Team
	}
	workers, aerr := s.scopeWorkers(u, siteID, team)
	if aerr != nil {
		return map[string]any{"month": month, "available": false}
	}
	dates := eachDate(from, to)
	ids := make([]int64, 0, len(workers))
	for _, wk := range workers {
		ids = append(ids, wk.ID)
	}
	punches, _ := s.loadPunches(siteID, ids, from, to)
	eligCache := map[int64]eligibility{}
	sum := &personAttendance{}
	noPunch, allVoid := 0, 0
	for i := range workers {
		wk := &workers[i]
		pa := s.buildAttendance(wk, dates, punches[wk.ID], eligCache)
		sum.ScheduledDays += pa.ScheduledDays
		sum.PresentDays += pa.PresentDays
		sum.AbsentDays += pa.AbsentDays
		sum.VoidedDays += pa.VoidedDays
		sum.LeaveDays += pa.LeaveDays
		sum.LateDays += pa.LateDays
		sum.EarlyDays += pa.EarlyDays
		sum.Hours += pa.Hours
		sum.ScheduledHours += pa.ScheduledHours
		sum.Workdays += pa.Workdays
		if !pa.HasAnyPunch {
			noPunch++
		}
		if pa.AllVoid {
			allVoid++
		}
	}
	// 已结算月被改动的差异数（提醒总包复核）
	var dirty int
	s.db.QueryRow(`SELECT COUNT(*) FROM payroll_items pi JOIN payroll_months pm ON pm.id=pi.payroll_month_id
		WHERE pm.site_id=? AND pi.dirty=1`, siteID).Scan(&dirty)

	return map[string]any{
		"available":       true,
		"month":           month,
		"from":            from,
		"to":              to,
		"people":          len(workers),
		"present_days":    sum.PresentDays,
		"scheduled_days":  sum.ScheduledDays,
		"absent_days":     sum.AbsentDays,
		"late_days":       sum.LateDays,
		"early_days":      sum.EarlyDays,
		"leave_days":      sum.LeaveDays,
		"voided_days":     sum.VoidedDays,
		"hours":           round2(sum.Hours),
		"scheduled_hours": round2(sum.ScheduledHours),
		"workdays":        round2(sum.Workdays),
		"rate_days_pct":   pct(float64(sum.PresentDays), float64(sum.ScheduledDays)),
		"rate_hours_pct":  pct(sum.Hours, sum.ScheduledHours),
		"no_punch_people": noPunch,
		"all_void_people": allVoid,
		"dirty_payroll":   dirty,
		"calibers":        []map[string]string{caliberMeta[CaliberDays], caliberMeta[CaliberHours]},
	}
}
