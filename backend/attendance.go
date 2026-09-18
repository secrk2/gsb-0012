package main

import (
	"database/sql"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ========== 出勤打卡：多源合并 → 日结论 → 甘特/双口径/修正留痕 ==========

// 日结论状态
const (
	DayPresent = "present" // 出勤
	DayLate    = "late"    // 迟到
	DayEarly   = "early"   // 早退
	DayAbsent  = "absent"  // 缺勤
	DayRest    = "rest"    // 休息（排班休息日，不算缺勤）
	DayVoid    = "void"    // 当日打卡全部作废
)

var dayStatusLabels = map[string]string{
	DayPresent: "出勤", DayLate: "迟到", DayEarly: "早退",
	DayAbsent: "缺勤", DayRest: "休息", DayVoid: "已作废",
}

func dayStatusLabel(s string) string {
	if l, ok := dayStatusLabels[s]; ok {
		return l
	}
	return s
}

var punchSourceLabels = map[string]string{"gate": "闸机", "mobile": "手机定位"}

// 双口径的统一文案（作战台 / 出勤详情 / 导出三处必须一致，禁止各写各的）
const caliberDaysText = "出勤率（按出勤天）= 实际出勤天 ÷ 应出勤天"
const caliberHoursText = "出勤率（按打卡工时）= 有效打卡工时 ÷ 排班工时"

type workRule struct {
	ID              int64
	SiteID          int64
	Team            string
	ShiftName       string
	InDeadline      string // HH:MM
	OutDeadline     string // HH:MM
	StandardMinutes int
	HalfDay         bool
	WorkDOW         map[int]bool
}

func (r *workRule) scheduled(t time.Time) bool { return r.WorkDOW[int(t.Weekday())] }

// 排班工日（半天班=0.5）
func (r *workRule) scheduledFactor() float64 {
	if r.HalfDay {
		return 0.5
	}
	return 1
}

func loadWorkRule(db interface {
	QueryRow(string, ...any) *sql.Row
}, siteID int64, team string) (*workRule, error) {
	r := &workRule{WorkDOW: map[int]bool{}}
	var inDead, outDead, dow string
	var stdHours float64
	var half int
	err := db.QueryRow(`
		SELECT id, site_id, team, shift_name, clock_in_deadline, clock_out_deadline,
		       standard_hours, half_day, work_dow
		FROM work_rules WHERE site_id = ? AND team IN (?, '*')
		ORDER BY CASE team WHEN ? THEN 0 ELSE 1 END LIMIT 1`,
		siteID, team, team).
		Scan(&r.ID, &r.SiteID, &r.Team, &r.ShiftName, &inDead, &outDead, &stdHours, &half, &dow)
	if err != nil {
		return nil, err
	}
	r.InDeadline = inDead
	r.OutDeadline = outDead
	r.StandardMinutes = int(stdHours*60 + 0.5)
	r.HalfDay = half == 1
	for _, p := range strings.Split(dow, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			r.WorkDOW[v] = true
		}
	}
	return r, nil
}

func hhmmToMin(s string) int {
	s = strings.TrimSpace(s)
	if len(s) >= 5 {
		if h, err := strconv.Atoi(s[:2]); err == nil {
			if m, err := strconv.Atoi(s[3:5]); err == nil {
				return h*60 + m
			}
		}
	}
	return -1
}

func parseDay(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", s, time.Local)
}

// ---------- 数据范围 ----------

type attScope struct {
	siteID   int64
	team     string // 仅该班组；空=全工地
	workerID int64  // 仅该工人；0=不限
	isWorker bool
	readOnly bool // 监管员
}

// resolveScope 解析当前用户可访问的工地/班组范围（与人员档案同一套隔离规则）
func (s *server) resolveScope(u *User, r *http.Request) (*attScope, *apiError) {
	q := r.URL.Query()
	sc := &attScope{}
	switch u.Role {
	case RoleRegulator:
		sc.readOnly = true
		siteID, aerr := s.resolveSiteID(u, r)
		if aerr != nil {
			return nil, aerr
		}
		sc.siteID = siteID
	case RoleGCAdmin, RoleSupervisor:
		if u.SiteID == nil {
			return nil, &apiError{HTTPCode: 403, Msg: "账号未绑定工地"}
		}
		sc.siteID = *u.SiteID
	case RoleSubLeader:
		if u.SiteID == nil {
			return nil, &apiError{HTTPCode: 403, Msg: "账号未绑定工地"}
		}
		sc.siteID = *u.SiteID
		sc.team = u.Team
	case RoleWorker:
		if u.WorkerID == nil {
			return nil, &apiError{HTTPCode: 403, Msg: "工人账号未关联人员档案"}
		}
		sc.isWorker = true
		sc.workerID = *u.WorkerID
		wk, err := s.getWorker(*u.WorkerID)
		if err != nil {
			return nil, &apiError{HTTPCode: 404, Msg: "人员档案不存在"}
		}
		sc.siteID = wk.SiteID
	default:
		return nil, &apiError{HTTPCode: 403, Msg: "当前角色无权访问出勤数据"}
	}
	// 显式参数收窄（监管员可切工地；总包/监理可筛班组）
	if v := q.Get("team"); v != "" {
		if u.Role == RoleSubLeader && v != u.Team {
			return nil, &apiError{HTTPCode: 403, Msg: "班组长只能查看本班组出勤"}
		}
		if !sc.isWorker {
			sc.team = v
		}
	}
	if v := q.Get("worker_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			if sc.isWorker && sc.workerID != id {
				return nil, &apiError{HTTPCode: 403, Msg: "工人只能查看本人出勤"}
			}
			sc.workerID = id
		}
	}
	return sc, nil
}

// ensureWorkerInScope 写操作前的目标工人鉴权
func (s *server) ensureWorkerInScope(u *User, workerID int64) (*Worker, *apiError) {
	wk, err := s.getWorker(workerID)
	if err == sql.ErrNoRows {
		return nil, &apiError{HTTPCode: 404, Msg: "人员档案不存在或已被删除"}
	}
	if err != nil {
		return nil, &apiError{HTTPCode: 500, Msg: "系统繁忙，请重试"}
	}
	if aerr := canViewWorker(u, wk); aerr != nil {
		return nil, aerr
	}
	return wk, nil
}

// ---------- 打卡合并与日结论判定 ----------

// recomputeDay 聚合某人某天的全部打卡（两条闸机 / 手机+闸机都只合成一条日结论）。
// 人工改判（manual=1）后不再被打卡自动覆盖。
func recomputeDay(tx *sql.Tx, workerID int64, day string) error {
	var manual int
	err := tx.QueryRow(`SELECT manual FROM attendance_days WHERE worker_id = ? AND day = ?`, workerID, day).Scan(&manual)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if manual == 1 {
		return nil
	}
	locked, _, lerr := dayLocked(tx, workerID, day)
	if lerr != nil {
		return lerr
	}

	rows, err := tx.Query(`
		SELECT punch_time, source, voided FROM attendance_punches
		WHERE worker_id = ? AND punch_date = ? ORDER BY punch_time, id`, workerID, day)
	if err != nil {
		return err
	}
	type punch struct {
		tm     string
		source string
		voided bool
	}
	var all []punch
	for rows.Next() {
		var tm, src string
		var v int
		if rows.Scan(&tm, &src, &v) == nil {
			all = append(all, punch{tm, src, v == 1})
		}
	}
	rows.Close()

	wk := &Worker{}
	if err := tx.QueryRow(`SELECT site_id, team FROM workers WHERE id = ?`, workerID).Scan(&wk.SiteID, &wk.Team); err != nil {
		return err
	}
	rule, err := loadWorkRule(tx, wk.SiteID, wk.Team)
	if err != nil {
		return err
	}
	t, err := parseDay(day)
	if err != nil {
		return err
	}

	status, firstIn, lastOut, sources := DayRest, "", "", ""
	validCnt, workMin := 0, 0
	srcCount := map[string]int{}
	if len(all) > 0 {
		var valid []punch
		for _, p := range all {
			if !p.voided {
				valid = append(valid, p)
				srcCount[p.source]++
			}
		}
		if len(valid) == 0 {
			status = DayVoid // 有打卡但全部作废
		} else {
			firstIn, lastOut = valid[0].tm, valid[len(valid)-1].tm
			fi, lo := hhmmToMin(firstIn), hhmmToMin(lastOut)
			if fi >= 0 && lo >= 0 && lo > fi {
				workMin = lo - fi
			}
			validCnt = len(valid)
			if !rule.scheduled(t) {
				status = DayPresent // 休息日出勤记为出勤（加班），仍计工日
			} else {
				inDead, outDead := hhmmToMin(rule.InDeadline), hhmmToMin(rule.OutDeadline)
				late := fi >= 0 && inDead >= 0 && fi > inDead
				early := lo >= 0 && outDead >= 0 && lo < outDead
				switch {
				case late && early:
					status = DayLate // 既迟到又早退，详情同时展示两标记
				case late:
					status = DayLate
				case early:
					status = DayEarly
				default:
					status = DayPresent
				}
			}
		}
		parts := []string{}
		for _, src := range []string{"gate", "mobile"} {
			if c := srcCount[src]; c > 0 {
				parts = append(parts, punchSourceLabels[src]+strconv.Itoa(c))
			}
		}
		sources = strings.Join(parts, "+")
	} else if rule.scheduled(t) {
		status = DayAbsent
	}

	allVoided := 0
	if status == DayVoid {
		allVoided = 1
	}
	_, err = tx.Exec(`
		INSERT INTO attendance_days(worker_id, day, status, first_in, last_out, sources,
			punch_count, valid_punch_count, work_minutes, all_voided, manual, locked, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,0,?,?)
		ON CONFLICT(worker_id, day) DO UPDATE SET
			status=excluded.status, first_in=excluded.first_in, last_out=excluded.last_out,
			sources=excluded.sources, punch_count=excluded.punch_count,
			valid_punch_count=excluded.valid_punch_count, work_minutes=excluded.work_minutes,
			all_voided=excluded.all_voided, updated_at=excluded.updated_at`,
		workerID, day, status, firstIn, lastOut, sources, len(all), validCnt, workMin, allVoided, locked, nowUTC())
	return err
}

// dayLocked 该日是否落在已结算（冻结）月
func dayLocked(tx *sql.Tx, workerID int64, day string) (bool, string, error) {
	if len(day) < 7 {
		return false, "", nil
	}
	month := day[:7]
	var siteID int64
	if err := tx.QueryRow(`SELECT site_id FROM workers WHERE id = ?`, workerID).Scan(&siteID); err != nil {
		return false, "", err
	}
	var st string
	err := tx.QueryRow(`SELECT status FROM payroll_months WHERE site_id = ? AND month = ?`, siteID, month).Scan(&st)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, month, nil
}

// ---------- 打卡上报 ----------

type punchReq struct {
	WorkerID   int64  `json:"worker_id"`
	PunchDate  string `json:"punch_date"`
	PunchTime  string `json:"punch_time"`
	Source     string `json:"source"`
	Device     string `json:"device"`
	Reason     string `json:"reason"`
	Confirm    bool   `json:"confirm"`
	ClientOpID string `json:"client_op_id"`
}

func (s *server) handlePunch(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == RoleRegulator {
		writeErr(w, http.StatusForbidden, "监管员账号为只读，不能上报打卡")
		return
	}
	var req punchReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Source = strings.TrimSpace(req.Source)
	if req.Source != "gate" && req.Source != "mobile" {
		writeErr(w, http.StatusBadRequest, "打卡来源仅支持 gate（闸机）/ mobile（手机定位）")
		return
	}
	if _, err := parseDay(req.PunchDate); err != nil {
		writeErr(w, http.StatusBadRequest, "打卡日期格式应为 YYYY-MM-DD")
		return
	}
	if hhmmToMin(req.PunchTime) < 0 {
		writeErr(w, http.StatusBadRequest, "打卡时间格式应为 HH:MM:SS")
		return
	}
	if req.WorkerID == 0 {
		writeErr(w, http.StatusBadRequest, "缺少打卡人员")
		return
	}
	// 工人账号只能给自己打手机定位卡
	if u.Role == RoleWorker {
		if u.WorkerID == nil || req.WorkerID != *u.WorkerID {
			writeErr(w, http.StatusForbidden, "工人只能为本人打卡")
			return
		}
		if req.Source != "mobile" {
			writeErr(w, http.StatusForbidden, "工人自助打卡仅支持手机定位")
			return
		}
	}
	wk, aerr := s.ensureWorkerInScope(u, req.WorkerID)
	if aerr != nil {
		aerr.write(w)
		return
	}
	if wk.Status == string(StatusBlacklisted) {
		writeErr(w, http.StatusConflict, "该人员已列入黑名单，不能打卡")
		return
	}
	// 补打已结算冻结月的卡：允许，但需二次确认+原因留痕，且不重算当月工资
	locked, month, _ := s.lockedInfo(req.WorkerID, req.PunchDate)
	if locked && !req.Confirm {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        fmt.Sprintf("打卡日期 %s 属于已结算冻结月份 %s，补打卡不会重算当月工资；请二次确认并填写原因留痕", req.PunchDate, month),
			"need_confirm": true, "locked_month": month,
		})
		return
	}
	if locked && strings.TrimSpace(req.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "补打已结算月份的卡必须填写原因（操作将留痕）")
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()

	duplicate := false
	punchID := int64(0)
	if req.ClientOpID != "" {
		if err := tx.QueryRow(`SELECT id FROM attendance_punches WHERE client_op_id = ?`, req.ClientOpID).Scan(&punchID); err == nil {
			duplicate = true // 同一终端重放：幂等，不重复入流水
		}
	}
	if !duplicate {
		device := req.Device
		if device == "" {
			if req.Source == "gate" {
				device = "闸机"
			} else {
				device = "手机定位"
			}
		}
		res, err := tx.Exec(`
			INSERT INTO attendance_punches(site_id, worker_id, punch_date, punch_time, source, device, client_op_id, created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			wk.SiteID, req.WorkerID, req.PunchDate, req.PunchTime, req.Source, device, nullableOp(req.ClientOpID), nowUTC())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "打卡失败，请重试")
			return
		}
		punchID, _ = res.LastInsertId()
		if locked {
			if err := s.writeCorrection(tx, u, req.WorkerID, req.PunchDate, "punch_locked", "", "补打卡", req.Reason); err != nil {
				writeErr(w, http.StatusInternalServerError, "留痕失败")
				return
			}
		}
		if err := recomputeDay(tx, req.WorkerID, req.PunchDate); err != nil {
			writeErr(w, http.StatusInternalServerError, "日结论合并失败")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "punch_id": punchID, "duplicate": duplicate,
		"notice": cond(duplicate, "该打卡此前已上报，已去重", "打卡成功；同日多台设备记录已自动合并为一条出勤"),
	})
}

// ---------- 作废 / 反作废 / 人工改判 ----------

type correctReq struct {
	Reason  string `json:"reason"`
	Confirm bool   `json:"confirm"`
	Status  string `json:"status"` // adjudicate 时的目标状态
}

func (s *server) writeCorrection(tx *sql.Tx, u *User, workerID int64, day, action, from, to, reason string) error {
	_, err := tx.Exec(`
		INSERT INTO attendance_corrections(worker_id, day, action, from_status, to_status, reason, actor_id, actor_name, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		workerID, day, action, from, to, strings.TrimSpace(reason), u.ID, u.Name, nowUTC())
	return err
}

func (s *server) handleVoidPunch(w http.ResponseWriter, r *http.Request, restore bool) {
	u := currentUser(r)
	if u.Role == RoleRegulator || u.Role == RoleWorker {
		writeErr(w, http.StatusForbidden, "当前角色无权限作废打卡")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "打卡编号格式错误")
		return
	}
	var req correctReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	var workerID int64
	var day string
	var voided int
	if err := s.db.QueryRow(`SELECT worker_id, punch_date, voided FROM attendance_punches WHERE id = ?`, id).
		Scan(&workerID, &day, &voided); err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "打卡记录不存在")
		return
	}

	wk, aerr := s.ensureWorkerInScope(u, workerID)
	if aerr != nil {
		aerr.write(w)
		return
	}
	_ = wk

	locked, month, _ := s.lockedInfo(workerID, day)
	if locked && !req.Confirm {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        fmt.Sprintf("该打卡属于已结算冻结月份 %s，作废/恢复不会重算当月工资；请二次确认并填写原因留痕", month),
			"need_confirm": true, "locked_month": month,
		})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "请填写作废/恢复原因（操作将留痕）")
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()
	target := 1
	action, from, to := "void_punch", "有效", "作废"
	if restore {
		target, action, from, to = 0, "restore_punch", "作废", "有效"
	}
	if _, err := tx.Exec(`UPDATE attendance_punches SET voided = ? WHERE id = ?`, target, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "操作失败")
		return
	}
	if err := s.writeCorrection(tx, u, workerID, day, action, from, to, req.Reason); err != nil {
		writeErr(w, http.StatusInternalServerError, "留痕失败")
		return
	}
	if err := recomputeDay(tx, workerID, day); err != nil {
		writeErr(w, http.StatusInternalServerError, "日结论重算失败")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "frozen_month_unchanged": locked,
		"notice": cond(locked, fmt.Sprintf("已处理并留痕；%s 月工资为冻结快照，未被重算", month), "已处理并留痕"),
	})
}

func (s *server) lockedInfo(workerID int64, day string) (bool, string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	return dayLocked(tx, workerID, day)
}

func (s *server) handleAdjudicate(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == RoleRegulator || u.Role == RoleWorker {
		writeErr(w, http.StatusForbidden, "当前角色无权人工改判出勤")
		return
	}
	var req struct {
		WorkerID int64  `json:"worker_id"`
		Day      string `json:"day"`
		Status   string `json:"status"`
		Reason   string `json:"reason"`
		Confirm  bool   `json:"confirm"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if _, ok := dayStatusLabels[req.Status]; !ok {
		writeErr(w, http.StatusBadRequest, "目标状态只能是 出勤/迟到/早退/缺勤/休息")
		return
	}
	if req.Status == DayVoid {
		writeErr(w, http.StatusBadRequest, "请通过作废打卡实现「已作废」状态")
		return
	}
	if _, err := parseDay(req.Day); err != nil {
		writeErr(w, http.StatusBadRequest, "日期格式应为 YYYY-MM-DD")
		return
	}
	if _, aerr := s.ensureWorkerInScope(u, req.WorkerID); aerr != nil {
		aerr.write(w)
		return
	}
	locked, month, _ := s.lockedInfo(req.WorkerID, req.Day)
	if locked && !req.Confirm {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        fmt.Sprintf("%s 属于已结算冻结月份 %s，改判不会重算当月工资；请二次确认并填写原因留痕", req.Day, month),
			"need_confirm": true, "locked_month": month,
		})
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "请填写改判原因（操作将留痕）")
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()
	var from string
	err = tx.QueryRow(`SELECT status FROM attendance_days WHERE worker_id = ? AND day = ?`, req.WorkerID, req.Day).Scan(&from)
	if err == sql.ErrNoRows {
		from = DayAbsent
	}
	if _, err := tx.Exec(`
		INSERT INTO attendance_days(worker_id, day, status, manual, locked, updated_at)
		VALUES(?,?,?,1,?,?)
		ON CONFLICT(worker_id, day) DO UPDATE SET status=excluded.status, manual=1, updated_at=excluded.updated_at`,
		req.WorkerID, req.Day, req.Status, b2i(locked), nowUTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, "改判失败")
		return
	}
	if err := s.writeCorrection(tx, u, req.WorkerID, req.Day, "adjudicate", from, req.Status, req.Reason); err != nil {
		writeErr(w, http.StatusInternalServerError, "留痕失败")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "frozen_month_unchanged": locked,
		"notice": cond(locked, fmt.Sprintf("已改判并留痕；%s 月工资为冻结快照，未被重算", month), "已改判并留痕，该日后将以人工结论为准"),
	})
}

// ---------- 甘特查询 ----------

func ganttRange(q url.Values) (time.Time, time.Time, string) {
	view := q.Get("view")
	if view == "" {
		view = "week"
	}
	today, _ := parseDay(todayLocal())
	if m := q.Get("month"); m != "" {
		if t, err := time.ParseInLocation("2006-01", m, time.Local); err == nil {
			from := t
			to := t.AddDate(0, 1, -1)
			return from, to, "month"
		}
	}
	if f := q.Get("from"); f != "" {
		if from, err := parseDay(f); err == nil {
			if t := q.Get("to"); t != "" {
				if to, err2 := parseDay(t); err2 == nil {
					return from, to, view
				}
			}
			return from, from.AddDate(0, 0, 6), view
		}
	}
	if view == "month" {
		from := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.Local)
		return from, from.AddDate(0, 1, -1), "month"
	}
	// 周：本周一到周日
	wd := int(today.Weekday())
	if wd == 0 {
		wd = 7
	}
	from := today.AddDate(0, 0, -(wd - 1))
	return from, from.AddDate(0, 0, 6), "week"
}

type dayCell struct {
	Day       string `json:"day"`
	Status    string `json:"status"`
	Label     string `json:"label"`
	FirstIn   string `json:"first_in"`
	LastOut   string `json:"last_out"`
	Sources   string `json:"sources"`
	WorkMin   int    `json:"work_minutes"`
	Valid     int    `json:"valid_punches"`
	Total     int    `json:"total_punches"`
	AllVoided bool   `json:"all_voided"`
	Manual    bool   `json:"manual"`
	Locked    bool   `json:"locked"`
	Scheduled bool   `json:"scheduled"`
	Late      bool   `json:"late_flag"`
	Early     bool   `json:"early_flag"`
}

// buildGantt 组装甘特：工人行 + 每日格子 + 双口径汇总
func (s *server) buildGantt(sc *attScope, from, to time.Time) (map[string]any, *apiError) {
	db := s.db
	// 工人范围：当前在册（按隔离条件）∪ 区间内有打卡/日结论者
	conds := []string{"site_id = ?"}
	args := []any{sc.siteID}
	if sc.team != "" {
		conds = append(conds, "team = ?")
		args = append(args, sc.team)
	}
	if sc.workerID != 0 {
		conds = append(conds, "id = ?")
		args = append(args, sc.workerID)
	}
	wc := strings.Join(conds, " AND ")
	rows, err := db.Query(`
		SELECT id, job_no, full_name, team, trade, status FROM workers
		WHERE `+wc+` ORDER BY team, job_no`, args...)
	if err != nil {
		return nil, &apiError{HTTPCode: 500, Msg: "查询失败"}
	}
	type wrow struct {
		id                                   int64
		jobNo, fullName, team, trade, status string
	}
	var workers []wrow
	for rows.Next() {
		var w wrow
		if rows.Scan(&w.id, &w.jobNo, &w.fullName, &w.team, &w.trade, &w.status) == nil {
			workers = append(workers, w)
		}
	}
	rows.Close()

	type ruleKey struct {
		site int64
		team string
	}
	ruleCache := map[ruleKey]*workRule{}
	ruleFor := func(team string) *workRule {
		k := ruleKey{sc.siteID, team}
		if r, ok := ruleCache[k]; ok {
			return r
		}
		r, err := loadWorkRule(db, sc.siteID, team)
		if err != nil {
			r = &workRule{WorkDOW: map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true}, StandardMinutes: 480}
		}
		ruleCache[k] = r
		return r
	}

	// 预取区间日结论
	daysMap := map[int64]map[string]*dayCell{}
	ids := make([]any, 0, len(workers))
	for _, w := range workers {
		ids = append(ids, w.id)
	}
	if len(ids) > 0 {
		qmarks := strings.Repeat("?,", len(ids))
		qmarks = qmarks[:len(qmarks)-1]
		drows, err := db.Query(`
			SELECT worker_id, day, status, first_in, last_out, sources, valid_punch_count,
			       punch_count, work_minutes, all_voided, manual, locked
			FROM attendance_days
			WHERE day BETWEEN ? AND ? AND worker_id IN (`+qmarks+`)`,
			append([]any{from.Format("2006-01-02"), to.Format("2006-01-02")}, ids...)...)
		if err != nil {
			return nil, &apiError{HTTPCode: 500, Msg: "查询失败"}
		}
		for drows.Next() {
			var wid int64
			var day, st, fin, lout, src string
			var valid, total, wm, allv, man, lock int
			if drows.Scan(&wid, &day, &st, &fin, &lout, &src, &valid, &total, &wm, &allv, &man, &lock) == nil {
				c := &dayCell{
					Day: day, Status: st, Label: dayStatusLabel(st), FirstIn: fin, LastOut: lout,
					Sources: src, Valid: valid, Total: total, WorkMin: wm,
					AllVoided: allv == 1, Manual: man == 1, Locked: lock == 1,
				}
				if daysMap[wid] == nil {
					daysMap[wid] = map[string]*dayCell{}
				}
				daysMap[wid][day] = c
			}
		}
		drows.Close()
	}

	dates := []string{}
	ruleSummary := map[string]any{}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		dates = append(dates, d.Format("2006-01-02"))
	}

	type wsum struct {
		scheduledDays, presentDays, overtimeDays float64
		scheduledMin, workedMin                  int
		hasPunch, allVoid                        bool
	}
	grand := wsum{}
	workerOut := []map[string]any{}
	for _, w := range workers {
		rule := ruleFor(w.team)
		cells := map[string]*dayCell{}
		sum := wsum{allVoid: true}
		for _, ds := range dates {
			t, _ := parseDay(ds)
			sched := rule.scheduled(t)
			c := daysMap[w.id][ds]
			if c == nil {
				st := DayRest
				if sched {
					st = DayAbsent
				}
				c = &dayCell{Day: ds, Status: st, Label: dayStatusLabel(st), Scheduled: sched}
			}
			c.Scheduled = sched
			// 迟到/早退双标记补算（迟到+早退同一天时，状态记迟到，标记同时亮）
			if c.FirstIn != "" && sched {
				inM, outM := hhmmToMin(c.FirstIn), hhmmToMin(c.LastOut)
				if inM >= 0 {
					c.Late = inM > hhmmToMin(rule.InDeadline)
				}
				if outM >= 0 && c.LastOut != "" {
					c.Early = outM < hhmmToMin(rule.OutDeadline)
				}
				if c.Late && c.Early && c.Status == DayLate {
					c.Label = "迟到+早退"
				}
			}
			cells[ds] = c

			if sched {
				sum.scheduledDays += rule.scheduledFactor()
				sum.scheduledMin += rule.StandardMinutes
			}
			if c.Total > 0 || c.AllVoided {
				sum.hasPunch = true
			}
			if c.Status != DayVoid && (c.Total > 0) {
				sum.allVoid = false
			}
			switch c.Status {
			case DayPresent, DayLate, DayEarly:
				if sched {
					// 按天口径：到岗即计实际出勤天（半天班按排班 0.5 计）
					sum.presentDays += rule.scheduledFactor()
				} else {
					// 休息日出勤另计为加班天，不进出勤率分子（分子只对应排班里的应到）
					sum.overtimeDays += 1
				}
			}
			sum.workedMin += c.WorkMin
		}
		// 应出勤口径：在场人员、或区间内确有打卡者（请假/退场/黑名单/待入场且无打卡不计应到，不算缺勤）
		eligible := w.status == string(StatusOnsite) || sum.hasPunch
		if eligible {
			grand.scheduledDays += sum.scheduledDays
			grand.scheduledMin += sum.scheduledMin
			grand.presentDays += sum.presentDays
			grand.workedMin += sum.workedMin
		}
		if !sum.hasPunch {
			sum.allVoid = false
		}
		rateDays := 0.0
		if sum.scheduledDays > 0 {
			rateDays = sum.presentDays / sum.scheduledDays
		}
		rateHours := 0.0
		if sum.scheduledMin > 0 {
			rateHours = float64(sum.workedMin) / float64(sum.scheduledMin)
			if rateHours > 1 {
				rateHours = 1
			}
		}
		workerOut = append(workerOut, map[string]any{
			"worker_id": w.id, "job_no": w.jobNo, "name": maskName(w.fullName),
			"team": w.team, "trade": w.trade, "worker_status": w.status,
			"days":          cells,
			"has_any_punch": sum.hasPunch, "all_voided": sum.hasPunch && sum.allVoid,
			"scheduled_days":    sum.scheduledDays,
			"present_days":      sum.presentDays,
			"rate_days":         rateDays,
			"scheduled_minutes": sum.scheduledMin,
			"worked_minutes":    sum.workedMin,
			"rate_hours":        rateHours,
			"half_day":          rule.HalfDay,
		})
	}

	rateDays := 0.0
	if grand.scheduledDays > 0 {
		rateDays = grand.presentDays / grand.scheduledDays
	}
	rateHours := 0.0
	if grand.scheduledMin > 0 {
		rateHours = float64(grand.workedMin) / float64(grand.scheduledMin)
		if rateHours > 1 {
			rateHours = 1
		}
	}

	// 规则摘要（半天班组在两口径下结论可能相反，界面必须显式说明）
	halfTeams := map[string]bool{}
	for _, w := range workers {
		if r := ruleFor(w.team); r.HalfDay {
			halfTeams[w.team] = true
		}
	}
	ruleSummary = map[string]any{
		"half_day_teams": mapKeys(halfTeams),
		"in_deadline":    "",
		"out_deadline":   "",
	}

	return map[string]any{
		"from": from.Format("2006-01-02"), "to": to.Format("2006-01-02"),
		"dates":   dates,
		"workers": workerOut,
		"calibers": map[string]any{
			"days":  map[string]any{"key": "days", "label": "按出勤天", "text": caliberDaysText, "value": rateDays},
			"hours": map[string]any{"key": "hours", "label": "按打卡工时", "text": caliberHoursText, "value": rateHours},
		},
		"total": map[string]any{
			"scheduled_days": grand.scheduledDays, "present_days": grand.presentDays,
			"rate_days": rateDays, "scheduled_minutes": grand.scheduledMin,
			"worked_minutes": grand.workedMin, "rate_hours": rateHours,
		},
		"rule": ruleSummary,
	}, nil
}

func (s *server) handleGantt(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sc, aerr := s.resolveScope(u, r)
	if aerr != nil {
		aerr.write(w)
		return
	}
	from, to, view := ganttRange(r.URL.Query())
	out, aerr2 := s.buildGantt(sc, from, to)
	if aerr2 != nil {
		aerr2.write(w)
		return
	}
	out["view"] = view
	writeJSON(w, http.StatusOK, out)
}

// ---------- 单人详情（含原始打卡 + 修正留痕） ----------

func (s *server) serveWorkerAttendance(w http.ResponseWriter, r *http.Request, workerID int64) {
	u := currentUser(r)
	wk, aerr := s.ensureWorkerInScope(u, workerID)
	if aerr != nil {
		aerr.write(w)
		return
	}
	q := r.URL.Query()
	from, to, _ := ganttRange(q)
	sc := &attScope{siteID: wk.SiteID, workerID: wk.ID, isWorker: u.Role == RoleWorker, readOnly: u.Role == RoleRegulator}
	out, aerr2 := s.buildGantt(sc, from, to)
	if aerr2 != nil {
		aerr2.write(w)
		return
	}

	// 原始打卡（含已作废），让「合并来源」可追溯
	punches := []map[string]any{}
	prows, err := s.db.Query(`
		SELECT id, punch_date, punch_time, source, device, voided, client_op_id IS NOT NULL, created_at
		FROM attendance_punches WHERE worker_id = ? AND punch_date BETWEEN ? AND ?
		ORDER BY punch_date DESC, punch_time DESC, id DESC`,
		workerID, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err == nil {
		for prows.Next() {
			var id int64
			var d, tm, src, dev, createdAt string
			var voided, hasOp int
			if prows.Scan(&id, &d, &tm, &src, &dev, &voided, &hasOp, &createdAt) == nil {
				punches = append(punches, map[string]any{
					"id": id, "date": d, "time": tm, "source": src,
					"source_label": punchSourceLabels[src], "device": dev,
					"voided": voided == 1, "created_at": createdAt,
				})
			}
		}
		prows.Close()
	}

	// 修正留痕
	corrs := []map[string]any{}
	crows, err := s.db.Query(`
		SELECT day, action, from_status, to_status, reason, actor_name, created_at
		FROM attendance_corrections WHERE worker_id = ? AND day BETWEEN ? AND ?
		ORDER BY id DESC LIMIT 100`,
		workerID, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err == nil {
		for crows.Next() {
			var day, action, fs, ts, reason, actor, at string
			if crows.Scan(&day, &action, &fs, &ts, &reason, &actor, &at) == nil {
				corrs = append(corrs, map[string]any{
					"day": day, "action": action,
					"from_label": dayStatusLabel(fs), "to_label": dayStatusLabel(ts),
					"reason": reason, "actor_name": actor, "created_at": at,
				})
			}
		}
		crows.Close()
	}

	rule, rerr := loadWorkRule(s.db, wk.SiteID, wk.Team)
	if rerr != nil {
		rule = &workRule{ShiftName: "默认白班", InDeadline: "08:00", OutDeadline: "17:30", StandardMinutes: 480}
	}
	canManage := u.Role != RoleWorker && u.Role != RoleRegulator
	out["worker"] = map[string]any{
		"id": wk.ID, "job_no": wk.JobNo, "name": cond(u.Role == RoleWorker, wk.FullName, maskName(wk.FullName)),
		"team": wk.Team, "trade": wk.Trade,
	}
	out["punches"] = punches
	out["corrections"] = corrs
	out["can_manage"] = canManage
	out["rule"] = map[string]any{
		"shift_name": rule.ShiftName, "in_deadline": rule.InDeadline, "out_deadline": rule.OutDeadline,
		"standard_hours": float64(rule.StandardMinutes) / 60, "half_day": rule.HalfDay,
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleWorkerAttendance(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "人员编号格式错误")
		return
	}
	s.serveWorkerAttendance(w, r, id)
}

func (s *server) handleMyAttendance(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role != RoleWorker || u.WorkerID == nil {
		writeErr(w, http.StatusBadRequest, "当前账号不是工人账号")
		return
	}
	s.serveWorkerAttendance(w, r, *u.WorkerID)
}

// ---------- CSV 导出（口径随表写出，与界面一致） ----------

func (s *server) handleAttendanceExport(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sc, aerr := s.resolveScope(u, r)
	if aerr != nil {
		aerr.write(w)
		return
	}
	from, to, _ := ganttRange(r.URL.Query())
	out, aerr2 := s.buildGantt(sc, from, to)
	if aerr2 != nil {
		aerr2.write(w)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="attendance_%s_%s.csv"`, from.Format("20060102"), to.Format("20060102")))
	w.Write([]byte{0xEF, 0xBB, 0xBF}) // Excel 中文 BOM
	cw := csv.NewWriter(w)

	total := out["total"].(map[string]any)
	pct := func(v any) string { return strconv.FormatFloat(anyF(v)*100, 'f', 1, 64) + "%" }
	_ = cw.Write([]string{"# 出勤打卡导出", from.Format("2006-01-02") + " 至 " + to.Format("2006-01-02")})
	_ = cw.Write([]string{"# " + caliberDaysText, pct(total["rate_days"])})
	_ = cw.Write([]string{"# " + caliberHoursText, pct(total["rate_hours"])})
	_ = cw.Write([]string{"# 半天班班组按 0.5 工日计应出勤/实际出勤；工时口径按实际首入-末出计算，两种口径可能不一致，均为本表正式口径"})
	_ = cw.Write(nil)

	_ = cw.Write([]string{"工号", "姓名", "班组", "日期", "星期", "日结论", "首次打卡", "末次打卡", "有效工时(小时)", "合并来源", "有效打卡数", "是否锁定", "改判"})
	weekCN := []string{"日", "一", "二", "三", "四", "五", "六"}
	for _, wm := range out["workers"].([]map[string]any) {
		for _, ds := range out["dates"].([]string) {
			c := wm["days"].(map[string]*dayCell)[ds]
			t, _ := parseDay(ds)
			_ = cw.Write([]string{
				wm["job_no"].(string), wm["name"].(string), wm["team"].(string),
				ds, "周" + weekCN[int(t.Weekday())], c.Label, c.FirstIn, c.LastOut,
				strconv.FormatFloat(float64(c.WorkMin)/60, 'f', 2, 64),
				c.Sources, strconv.Itoa(c.Valid),
				cond(c.Locked, "已结算冻结", ""), cond(c.Manual, "人工改判", ""),
			})
		}
	}
	_ = cw.Write(nil)
	_ = cw.Write([]string{"工号", "姓名", "班组", "应出勤天(含半天折算)", "实际出勤天", caliberDaysText, "排班工时(小时)", "有效打卡工时(小时)", caliberHoursText})
	for _, wm := range out["workers"].([]map[string]any) {
		_ = cw.Write([]string{
			wm["job_no"].(string), wm["name"].(string), wm["team"].(string),
			strconv.FormatFloat(anyF(wm["scheduled_days"]), 'f', 1, 64),
			strconv.FormatFloat(anyF(wm["present_days"]), 'f', 1, 64),
			pct(wm["rate_days"]),
			strconv.FormatFloat(float64(anyI(wm["scheduled_minutes"]))/60, 'f', 1, 64),
			strconv.FormatFloat(float64(anyI(wm["worked_minutes"]))/60, 'f', 1, 64),
			pct(wm["rate_hours"]),
		})
	}
	cw.Flush()
}

// ---------- 小工具 ----------

func nullableOp(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func cond(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func anyF(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

func anyI(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}
