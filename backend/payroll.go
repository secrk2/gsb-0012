package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ========== 工资分账：合同版本单价 × 出勤工日，结算冻结、部分发薪不冲销 ==========

type payrollScope struct {
	siteID   int64
	team     string
	workerID int64
	isWorker bool
	readOnly bool
}

func (s *server) resolvePayrollScope(u *User, r *http.Request) (*payrollScope, *apiError) {
	sc := &payrollScope{}
	q := r.URL.Query()
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
		return nil, &apiError{HTTPCode: 403, Msg: "当前角色无权访问分账数据"}
	}
	if v := q.Get("team"); v != "" && !sc.isWorker {
		if u.Role == RoleSubLeader && v != u.Team {
			return nil, &apiError{HTTPCode: 403, Msg: "班组长只能查看本班组分账"}
		}
		sc.team = v
	}
	if v := q.Get("worker_id"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			if sc.isWorker && sc.workerID != id {
				return nil, &apiError{HTTPCode: 403, Msg: "工人只能查看本人分账"}
			}
			sc.workerID = id
		}
	}
	return sc, nil
}

// ---------- 合同版本 ----------

type contractVer struct {
	ID               int64   `json:"id"`
	SiteID           int64   `json:"site_id"`
	Team             string  `json:"team"`
	DailyRate        float64 `json:"daily_rate"`
	HalfRateFraction float64 `json:"half_rate_fraction"`
	ValidFrom        string  `json:"valid_from"`
	Note             string  `json:"note"`
	CreatedBy        string  `json:"created_by"`
	CreatedAt        string  `json:"created_at"`
}

// effectiveRate 某日生效的合同版本（valid_from <= day 取最新）
func effectiveRate(q queryer, siteID int64, team, day string) (contractVer, error) {
	var cv contractVer
	err := q.QueryRow(`
		SELECT id, site_id, team, daily_rate, half_rate_fraction, valid_from, note, created_by, created_at
		FROM contract_versions
		WHERE site_id = ? AND team = ? AND valid_from <= ?
		ORDER BY valid_from DESC, id DESC LIMIT 1`,
		siteID, team, day).Scan(&cv.ID, &cv.SiteID, &cv.Team, &cv.DailyRate, &cv.HalfRateFraction,
		&cv.ValidFrom, &cv.Note, &cv.CreatedBy, &cv.CreatedAt)
	return cv, err
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func (s *server) handleListContracts(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sc, aerr := s.resolvePayrollScope(u, r)
	if aerr != nil {
		aerr.write(w)
		return
	}
	q := r.URL.Query()
	team := sc.team
	if v := q.Get("team"); v != "" {
		if u.Role == RoleSubLeader && v != u.Team {
			writeErr(w, http.StatusForbidden, "班组长只能查看本班组合同")
			return
		}
		team = v
	}
	query := `SELECT id, site_id, team, daily_rate, half_rate_fraction, valid_from, note, created_by, created_at
	          FROM contract_versions WHERE site_id = ?`
	args := []any{sc.siteID}
	if team != "" {
		query += " AND team = ?"
		args = append(args, team)
	}
	query += " ORDER BY team, valid_from DESC, id DESC"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	defer rows.Close()
	out := []contractVer{}
	for rows.Next() {
		var cv contractVer
		if rows.Scan(&cv.ID, &cv.SiteID, &cv.Team, &cv.DailyRate, &cv.HalfRateFraction,
			&cv.ValidFrom, &cv.Note, &cv.CreatedBy, &cv.CreatedAt) == nil {
			out = append(out, cv)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"contracts": out, "server_time": nowUTC()})
}

type contractReq struct {
	Team             string  `json:"team"`
	DailyRate        float64 `json:"daily_rate"`
	HalfRateFraction float64 `json:"half_rate_fraction"`
	ValidFrom        string  `json:"valid_from"`
	Note             string  `json:"note"`
}

func (s *server) handleCreateContract(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role != RoleGCAdmin {
		writeErr(w, http.StatusForbidden, "仅总包管理员可调整合同日单价版本")
		return
	}
	var req contractReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Team = strings.TrimSpace(req.Team)
	if req.Team == "" {
		writeErr(w, http.StatusBadRequest, "请选择班组")
		return
	}
	if req.DailyRate <= 0 {
		writeErr(w, http.StatusBadRequest, "日单价必须大于 0")
		return
	}
	if req.HalfRateFraction <= 0 {
		req.HalfRateFraction = 1.0
	}
	if _, err := parseDay(req.ValidFrom); err != nil {
		writeErr(w, http.StatusBadRequest, "生效日期格式应为 YYYY-MM-DD")
		return
	}
	// 班组长班组越权校验
	if u.SiteID == nil {
		writeErr(w, http.StatusForbidden, "账号未绑定工地")
		return
	}
	res, err := s.db.Exec(`
		INSERT INTO contract_versions(site_id, team, daily_rate, half_rate_fraction, valid_from, note, created_by, created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		*u.SiteID, req.Team, req.DailyRate, req.HalfRateFraction, req.ValidFrom,
		strings.TrimSpace(req.Note), u.Name, nowUTC())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败")
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id,
		"notice": fmt.Sprintf(
			"新单价 %.0f 元/工日自 %s 起生效；此前已结算月份冻结在原单价版本，不受影响，仅 %s 之后的出勤按新单价计算",
			req.DailyRate, req.ValidFrom, req.ValidFrom),
	})
}

// ---------- 工日/金额试算 ----------

type linePreview struct {
	WorkerID      int64   `json:"worker_id"`
	JobNo         string  `json:"job_no"`
	Name          string  `json:"name"`
	Team          string  `json:"team"`
	WorkDays      float64 `json:"work_days"`
	PresentDays   float64 `json:"present_days"`
	LateDays      int     `json:"late_days"`
	EarlyDays     int     `json:"early_days"`
	AbsentDays    int     `json:"absent_days"`
	DailyRate     float64 `json:"daily_rate"`
	RateValidFrom string  `json:"rate_valid_from"`
	Amount        float64 `json:"amount"`
}

func monthBounds(month string) (time.Time, time.Time, error) {
	from, err := time.ParseInLocation("2006-01", month, time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	to := from.AddDate(0, 1, -1)
	return from, to, nil
}

// buildPreview 按「每个出勤日取当日生效版本」逐天计价：新版本只影响其生效日之后
func (s *server) buildPreview(sc *payrollScope, month string) ([]linePreview, float64, *apiError) {
	return s.buildPreviewQ(s.db, sc, month)
}

func (s *server) buildPreviewTx(tx *sql.Tx, sc *payrollScope, month string) ([]linePreview, float64, *apiError) {
	return s.buildPreviewQ(tx, sc, month)
}

func (s *server) buildPreviewQ(q queryer, sc *payrollScope, month string) ([]linePreview, float64, *apiError) {
	from, to, err := monthBounds(month)
	if err != nil {
		return nil, 0, &apiError{HTTPCode: 400, Msg: "月份格式应为 YYYY-MM"}
	}
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
	rows, err := q.Query(`
		SELECT id, job_no, full_name, team FROM workers WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY team, job_no`, args...)
	if err != nil {
		return nil, 0, &apiError{HTTPCode: 500, Msg: "查询失败"}
	}
	type wr struct {
		id                int64
		jobNo, name, team string
	}
	var ws []wr
	for rows.Next() {
		var x wr
		if rows.Scan(&x.id, &x.jobNo, &x.name, &x.team) == nil {
			ws = append(ws, x)
		}
	}
	rows.Close()

	fromS, toS := from.Format("2006-01-02"), to.Format("2006-01-02")
	previews := []linePreview{}
	var grand float64
	for _, x := range ws {
		rule, rerr := loadWorkRule(q, sc.siteID, x.team)
		if rerr != nil {
			rule = &workRule{StandardMinutes: 480}
		}
		dayStatus := map[string]string{}
		arows, err := q.Query(`
			SELECT day, status FROM attendance_days
			WHERE worker_id = ? AND day BETWEEN ? AND ?`, x.id, fromS, toS)
		if err != nil {
			return nil, 0, &apiError{HTTPCode: 500, Msg: "查询失败"}
		}
		for arows.Next() {
			var d, st string
			if arows.Scan(&d, &st) == nil {
				dayStatus[d] = st
			}
		}
		arows.Close()

		pv := linePreview{WorkerID: x.id, JobNo: x.jobNo, Name: maskName(x.name), Team: x.team}
		rateUsed := contractVer{DailyRate: 0, HalfRateFraction: 1}
		for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
			ds := d.Format("2006-01-02")
			st, ok := dayStatus[ds]
			if !ok {
				continue // 无日结论行（缺勤/休息均为 0 工日，不计钱）
			}
			factor := rule.scheduledFactor()
			switch st {
			case DayPresent, DayLate:
				if st == DayLate {
					pv.LateDays++
				}
				if rate, e := effectiveRate(q, sc.siteID, x.team, ds); e == nil {
					rateUsed = rate
				}
				pv.WorkDays += factor
				pv.PresentDays += factor
				pv.Amount += round2(factor * rateUsed.DailyRate)
			case DayEarly:
				pv.EarlyDays++
				if rate, e := effectiveRate(q, sc.siteID, x.team, ds); e == nil {
					rateUsed = rate
				}
				earlyFactor := factor * rateUsed.HalfRateFraction
				pv.WorkDays += earlyFactor
				pv.PresentDays += earlyFactor
				pv.Amount += round2(earlyFactor * rateUsed.DailyRate)
			case DayAbsent:
				pv.AbsentDays++
			}
		}
		pv.DailyRate = rateUsed.DailyRate
		pv.RateValidFrom = rateUsed.ValidFrom
		pv.WorkDays = round2(pv.WorkDays)
		pv.PresentDays = round2(pv.PresentDays)
		pv.Amount = round2(pv.Amount)
		if pv.WorkDays > 0 || pv.AbsentDays > 0 {
			previews = append(previews, pv)
			grand += pv.Amount
		}
	}
	sort.SliceStable(previews, func(i, j int) bool { return previews[i].JobNo < previews[j].JobNo })
	return previews, round2(grand), nil
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// ---------- 月分账查询（冻结快照 / 开放月试算） ----------

func (s *server) handlePayrollMonth(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sc, aerr := s.resolvePayrollScope(u, r)
	if aerr != nil {
		aerr.write(w)
		return
	}
	month := r.URL.Query().Get("month")
	if month == "" {
		month = todayLocal()[:7]
	}
	if _, _, err := monthBounds(month); err != nil {
		writeErr(w, http.StatusBadRequest, "月份格式应为 YYYY-MM")
		return
	}

	var pmID int64
	var status, frozenNote, settledAt, settledBy string
	var totalAmount, paidAmount float64
	err := s.db.QueryRow(`
		SELECT id, status, total_amount, paid_amount, frozen_note, settled_at, settled_by
		FROM payroll_months WHERE site_id = ? AND month = ?`, sc.siteID, month).
		Scan(&pmID, &status, &totalAmount, &paidAmount, &frozenNote, &settledAt, &settledBy)

	frozen := err == nil
	if err != nil && err != sql.ErrNoRows {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}

	lines := []map[string]any{}
	if frozen {
		// 差额合计用聚合子查询一次带出，避免在 Rows 游标上再开查询（SQLite 单连接会自死锁）
		lq := `SELECT l.worker_id, w.job_no, w.full_name, l.team, l.work_days, l.daily_rate,
		              l.rate_version_date, l.amount, l.paid_amount, l.status, COALESCE(a.adj, 0)
		       FROM payroll_lines l JOIN workers w ON w.id = l.worker_id
		       LEFT JOIN (SELECT month_id, worker_id, SUM(amount) adj
		                  FROM payroll_adjustments GROUP BY month_id, worker_id) a
		         ON a.month_id = l.month_id AND a.worker_id = l.worker_id
		       WHERE l.month_id = ?`
		largs := []any{pmID}
		if sc.team != "" {
			lq += " AND l.team = ?"
			largs = append(largs, sc.team)
		}
		if sc.workerID != 0 {
			lq += " AND l.worker_id = ?"
			largs = append(largs, sc.workerID)
		}
		lq += " ORDER BY l.team, w.job_no"
		lrows, err := s.db.Query(lq, largs...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查询失败")
			return
		}
		type lrow struct {
			wid                                int64
			jobNo, fullName, team, verDate, st string
			workDays, rate, amount, paid, adj  float64
		}
		var lrs []lrow
		for lrows.Next() {
			var x lrow
			if lrows.Scan(&x.wid, &x.jobNo, &x.fullName, &x.team, &x.workDays, &x.rate,
				&x.verDate, &x.amount, &x.paid, &x.st, &x.adj) == nil {
				lrs = append(lrs, x)
			}
		}
		lrows.Close()
		payableGrand := 0.0
		for _, x := range lrs {
			name := maskName(x.fullName)
			if sc.isWorker {
				name = x.fullName
			}
			payable := round2(x.amount + x.adj)
			payableGrand += payable
			lines = append(lines, map[string]any{
				"worker_id": x.wid, "job_no": x.jobNo, "name": name, "team": x.team,
				"work_days": x.workDays, "daily_rate": x.rate, "rate_version_date": x.verDate,
				"amount": x.amount, "adjustment": round2(x.adj),
				"payable":     payable,
				"paid_amount": round2(x.paid),
				"remaining":   round2(x.amount + x.adj - x.paid),
				"status":      x.st,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"month": month, "frozen": true, "status": status,
			"total_amount": round2(payableGrand), "frozen_base_amount": round2(totalAmount),
			"paid_amount": round2(paidAmount),
			"remaining":   round2(payableGrand - paidAmount),
			"frozen_note": frozenNote, "settled_at": settledAt, "settled_by": settledBy,
			"lines": lines, "can_pay": !sc.readOnly && !sc.isWorker,
			"can_adjust": !sc.readOnly && !sc.isWorker,
			"can_reopen": false, // 冻结月不开放撤销，统一走差额调整
		})
		return
	}

	// 未结算：实时试算（取当日最新合同版本）
	previews, grand, aerr2 := s.buildPreview(sc, month)
	if aerr2 != nil {
		aerr2.write(w)
		return
	}
	for _, p := range previews {
		lines = append(lines, map[string]any{
			"worker_id": p.WorkerID, "job_no": p.JobNo, "name": p.Name, "team": p.Team,
			"work_days": p.WorkDays, "present_days": p.PresentDays,
			"late_days": p.LateDays, "early_days": p.EarlyDays, "absent_days": p.AbsentDays,
			"daily_rate": p.DailyRate, "rate_version_date": p.RateValidFrom,
			"amount": p.Amount, "payable": p.Amount, "paid_amount": 0.0,
			"remaining": p.Amount, "status": "preview",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"month": month, "frozen": false, "status": "open",
		"total_amount": grand, "paid_amount": 0.0, "remaining": grand,
		"lines": lines, "can_settle": !sc.readOnly && !sc.isWorker,
		"notice": "未结算月份按当前合同版本实时试算；结算后将冻结为快照，之后单价调整或出勤修正均不重算本月",
	})
}

// 月历：近月状态（供前端月切换）
func (s *server) handlePayrollMonths(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sc, aerr := s.resolvePayrollScope(u, r)
	if aerr != nil {
		aerr.write(w)
		return
	}
	rows, err := s.db.Query(`
		SELECT month, status, total_amount, paid_amount FROM payroll_months
		WHERE site_id = ? ORDER BY month DESC`, sc.siteID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var month, status string
		var total, paid float64
		if rows.Scan(&month, &status, &total, &paid) == nil {
			out = append(out, map[string]any{
				"month": month, "status": status,
				"status_label": map[string]string{"open": "未结算", "settled": "已结算待发", "partial": "部分已发", "paid": "已发清"}[status],
				"total_amount": round2(total), "paid_amount": round2(paid),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"months": out})
}

// ---------- 结算（冻结快照） ----------

type settleReq struct {
	Month string `json:"month"`
	Note  string `json:"note"`
}

func (s *server) handleSettle(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == RoleWorker || u.Role == RoleRegulator {
		writeErr(w, http.StatusForbidden, "当前角色无权结算分账")
		return
	}
	var req settleReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	from, to, err := monthBounds(req.Month)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "月份格式应为 YYYY-MM")
		return
	}
	sc := &payrollScope{siteID: *u.SiteID}
	if u.Role == RoleSubLeader {
		writeErr(w, http.StatusForbidden, "分账结算由总包统一办理，班组长可查看本班组明细")
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()

	var existID int64
	var existStatus string
	var existPaid float64
	err = tx.QueryRow(`SELECT id, status, paid_amount FROM payroll_months WHERE site_id=? AND month=?`,
		sc.siteID, req.Month).Scan(&existID, &existStatus, &existPaid)
	if err == nil {
		writeErr(w, http.StatusConflict, fmt.Sprintf("%s 月已结算（%s），快照已冻结，不能重复结算；如需改判出勤请走「差额调整」", req.Month,
			map[string]string{"settled": "待发薪", "partial": "部分已发", "paid": "已发清"}[existStatus]))
		return
	}
	if err != sql.ErrNoRows {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}

	previews, grand, aerr2 := s.buildPreviewTx(tx, sc, req.Month)
	if aerr2 != nil {
		aerr2.write(w)
		return
	}
	res, err := tx.Exec(`
		INSERT INTO payroll_months(site_id, month, status, total_amount, paid_amount, frozen_note, settled_at, settled_by, created_at)
		VALUES(?,?, 'settled', ?, 0, ?, ?, ?, ?)`,
		sc.siteID, req.Month, grand, strings.TrimSpace(req.Note), nowUTC(), u.Name, nowUTC())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "结算失败")
		return
	}
	monthID, _ := res.LastInsertId()

	for _, p := range previews {
		if _, err := tx.Exec(`
			INSERT INTO payroll_lines(month_id, worker_id, team, work_days, daily_rate, rate_version_date, amount, paid_amount, status)
			VALUES(?,?,?,?,?,?,?,0,'unpaid')`,
			monthID, p.WorkerID, p.Team, p.WorkDays, p.DailyRate, p.RateValidFrom, p.Amount); err != nil {
			writeErr(w, http.StatusInternalServerError, "写入快照失败")
			return
		}
	}

	// 冻结区间出勤：加锁并留痕（结算锁定，不改变出勤结论）
	if _, err := tx.Exec(`UPDATE attendance_days SET locked = 1 WHERE day BETWEEN ? AND ?
		AND worker_id IN (SELECT id FROM workers WHERE site_id = ?)`,
		from.Format("2006-01-02"), to.Format("2006-01-02"), sc.siteID); err != nil {
		writeErr(w, http.StatusInternalServerError, "锁定出勤失败")
		return
	}
	if _, err := tx.Exec(`
		INSERT INTO attendance_corrections(worker_id, day, action, from_status, to_status, reason, actor_id, actor_name, created_at)
		SELECT id, ?, 'settle_lock', '', '', ?, ?, ?, ? FROM workers WHERE site_id = ?`,
		req.Month+"-01", fmt.Sprintf("分账月 %s 结算冻结", req.Month), u.ID, u.Name, nowUTC(), sc.siteID); err != nil {
		writeErr(w, http.StatusInternalServerError, "锁定留痕失败")
		return
	}

	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "month": req.Month, "month_id": monthID,
		"total_amount": grand, "line_count": len(previews),
		"notice": fmt.Sprintf("%s 月已结算冻结，共 %d 人、%.2f 元；此后新单价与出勤修正均不影响本月", req.Month, len(previews), grand),
	})
}

// ---------- 发薪（支持部分发，已发只增不减） ----------

type payReq struct {
	Month    string  `json:"month"`
	WorkerID int64   `json:"worker_id"`
	Amount   float64 `json:"amount"` // 0 或省略 = 发清该人剩余；批量发清可不传 worker_id
	Note     string  `json:"note"`
	PayAll   bool    `json:"pay_all"`
}

func (s *server) handlePay(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == RoleWorker || u.Role == RoleRegulator {
		writeErr(w, http.StatusForbidden, "当前角色无权发薪")
		return
	}
	if u.Role == RoleSubLeader {
		writeErr(w, http.StatusForbidden, "发薪由总包统一办理，班组长可查看本班组明细")
		return
	}
	var req payReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if u.SiteID == nil {
		writeErr(w, http.StatusForbidden, "账号未绑定工地")
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()

	var monthID int64
	var mstatus string
	var totalPaid float64
	if err := tx.QueryRow(`SELECT id, status, paid_amount FROM payroll_months WHERE site_id=? AND month=?`,
		*u.SiteID, req.Month).Scan(&monthID, &mstatus, &totalPaid); err != nil {
		writeErr(w, http.StatusConflict, req.Month+" 月尚未结算，不能发薪；请先结算冻结")
		return
	}

	type target struct {
		lineID        int64
		workerID      int64
		payable, paid float64
	}
	var targets []target
	q := `SELECT l.id, l.worker_id, l.amount + COALESCE(a.adj,0), l.paid_amount
	      FROM payroll_lines l
	      LEFT JOIN (SELECT month_id, worker_id, SUM(amount) adj
	                 FROM payroll_adjustments GROUP BY month_id, worker_id) a
	        ON a.month_id = l.month_id AND a.worker_id = l.worker_id
	      WHERE l.month_id = ?`
	qargs := []any{monthID}
	if req.WorkerID != 0 {
		q += " AND l.worker_id = ?"
		qargs = append(qargs, req.WorkerID)
	}
	lrows, err := tx.Query(q, qargs...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	for lrows.Next() {
		var t target
		if lrows.Scan(&t.lineID, &t.workerID, &t.payable, &t.paid) == nil {
			targets = append(targets, t)
		}
	}
	lrows.Close()
	if len(targets) == 0 {
		writeErr(w, http.StatusNotFound, "没有匹配的分账明细")
		return
	}
	if !req.PayAll && req.WorkerID == 0 {
		writeErr(w, http.StatusBadRequest, "请指定发薪人员，或使用批量发清")
		return
	}

	paidTotal := 0.0
	paidN := 0
	for _, t := range targets {
		remaining := round2(t.payable - t.paid)
		if remaining <= 0.001 {
			continue
		}
		payAmount := remaining
		if req.WorkerID != 0 && req.Amount > 0 {
			payAmount = round2(req.Amount)
		}
		if payAmount <= 0 {
			writeErr(w, http.StatusBadRequest, "发薪金额必须大于 0")
			return
		}
		if payAmount > remaining+0.001 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("本次发放 %.2f 元超过待发 %.2f 元；已发金额不会被冲减，请勿超额发放", payAmount, remaining))
			return
		}
		if _, err := tx.Exec(`
			INSERT INTO payroll_payments(month_id, worker_id, amount, note, actor_name, created_at)
			VALUES(?,?,?,?,?,?)`, monthID, t.workerID, payAmount, strings.TrimSpace(req.Note), u.Name, nowUTC()); err != nil {
			writeErr(w, http.StatusInternalServerError, "发薪失败")
			return
		}
		newPaid := round2(t.paid + payAmount)
		lstatus := "partial"
		if newPaid >= t.payable-0.001 {
			lstatus = "paid"
		}
		if _, err := tx.Exec(`UPDATE payroll_lines SET paid_amount = ?, status = ? WHERE id = ?`, newPaid, lstatus, t.lineID); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新明细失败")
			return
		}
		paidTotal += payAmount
		paidN++
		if req.WorkerID != 0 && req.Amount > 0 {
			break // 单人指定金额只处理一笔
		}
	}

	// 月状态：全部发清→paid，否则 partial
	var sumAmount, sumPaid float64
	tx.QueryRow(`SELECT COALESCE(SUM(l.amount),0)+COALESCE((SELECT SUM(amount) FROM payroll_adjustments WHERE month_id=?),0),
	                    COALESCE(SUM(l.paid_amount),0) FROM payroll_lines l WHERE l.month_id=?`, monthID, monthID).
		Scan(&sumAmount, &sumPaid)
	mst := "partial"
	if sumPaid >= sumAmount-0.001 && sumAmount > 0 {
		mst = "paid"
	}
	if _, err := tx.Exec(`UPDATE payroll_months SET paid_amount = ?, status = ? WHERE id = ?`, round2(sumPaid), mst, monthID); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新月份失败")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "paid_count": paidN, "paid_amount": round2(paidTotal),
		"month_status": mst,
		"notice":       fmt.Sprintf("已发放 %d 人共 %.2f 元；发薪流水只增不减，后续任何重算都不会冲销已发部分", paidN, paidTotal),
	})
}

// ---------- 差额调整（冻结月改出勤后的补差/扣减，有留痕） ----------

type adjustReq struct {
	Month    string  `json:"month"`
	WorkerID int64   `json:"worker_id"`
	Amount   float64 `json:"amount"` // 正=补，负=扣
	Reason   string  `json:"reason"`
}

func (s *server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role != RoleGCAdmin && u.Role != RoleSupervisor {
		writeErr(w, http.StatusForbidden, "仅总包管理员/监理可登记差额调整")
		return
	}
	var req adjustReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.WorkerID == 0 {
		writeErr(w, http.StatusBadRequest, "请指定人员")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "请填写差额原因（将与出勤修正一并留痕）")
		return
	}
	if u.SiteID == nil {
		writeErr(w, http.StatusForbidden, "账号未绑定工地")
		return
	}
	var monthID int64
	var frozenAmount, paid float64
	err := s.db.QueryRow(`
		SELECT m.id, l.amount, l.paid_amount
		FROM payroll_months m JOIN payroll_lines l ON l.month_id = m.id
		WHERE m.site_id = ? AND m.month = ? AND l.worker_id = ?`,
		*u.SiteID, req.Month, req.WorkerID).Scan(&monthID, &frozenAmount, &paid)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "该月分账明细不存在（未结算月份无需调整，直接重算即可）")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	var adjSum float64
	s.db.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM payroll_adjustments WHERE month_id=? AND worker_id=?`, monthID, req.WorkerID).Scan(&adjSum)
	newPayable := round2(frozenAmount + adjSum + req.Amount)
	if newPayable < paid-0.001 {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"调整后应付 %.2f 元将低于已发 %.2f 元；已发部分不能被冲销，请改为较小扣减或先行线下追回后登记", newPayable, paid))
		return
	}
	if _, err := s.db.Exec(`
		INSERT INTO payroll_adjustments(month_id, worker_id, amount, reason, actor_name, created_at)
		VALUES(?,?,?,?,?,?)`, monthID, req.WorkerID, req.Amount, strings.TrimSpace(req.Reason), u.Name, nowUTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, "登记失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "new_payable": newPayable, "paid_amount": round2(paid),
		"remaining": round2(newPayable - paid),
		"notice":    "已登记差额；原冻结金额与已发金额均未改动，差额单列留痕",
	})
}
