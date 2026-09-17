package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ========== 工资分账 ==========
//
// 规则：
//  - 月工资 = 出勤工日 × 日单价；工日来自出勤判定（有效工时/8）。
//  - 日单价随合同版本变：取 effective_from <= 当月 的最新版本；新版本只影响之后月份。
//  - 结算瞬间把 工日/单价版本/单价/金额 快照进 payroll_items，历史月冻结在原单价版本。
//  - payroll_payments 是独立发放流水：部分发放后任何重算/复核都不冲销已发部分。
//  - 结算月打卡被改动：原金额不动，只打 dirty 并给出实时对照金额，由总包二次确认+原因后补差。

type contractVer struct {
	ID            int64   `json:"id"`
	WorkerID      int64   `json:"worker_id"`
	Version       int     `json:"version"`
	DayRate       float64 `json:"day_rate"`
	EffectiveFrom string  `json:"effective_from"`
	Note          string  `json:"note"`
	CreatedByName string  `json:"created_by_name"`
	CreatedAt     string  `json:"created_at"`
}

type liveSummary struct {
	Workdays   float64
	Hours      float64
	RateVer    int
	DayRate    float64
	Amount     float64
	ContractID int64
}

func (s *server) activeContract(workerID int64, month string) (*contractVer, error) {
	c := &contractVer{}
	err := s.db.QueryRow(`
		SELECT id, worker_id, version, day_rate, effective_from,
		       COALESCE(note,''), COALESCE(created_by_name,''), created_at
		FROM worker_contracts
		WHERE worker_id = ? AND effective_from <= ?
		ORDER BY effective_from DESC, version DESC LIMIT 1`, workerID, month+"-01").
		Scan(&c.ID, &c.WorkerID, &c.Version, &c.DayRate, &c.EffectiveFrom, &c.Note, &c.CreatedByName, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (s *server) listContracts(workerID int64) []contractVer {
	rows, err := s.db.Query(`
		SELECT id, worker_id, version, day_rate, effective_from,
		       COALESCE(note,''), COALESCE(created_by_name,''), created_at
		FROM worker_contracts WHERE worker_id = ? ORDER BY version DESC`, workerID)
	if err != nil {
		return []contractVer{}
	}
	defer rows.Close()
	out := []contractVer{}
	for rows.Next() {
		var c contractVer
		if rows.Scan(&c.ID, &c.WorkerID, &c.Version, &c.DayRate, &c.EffectiveFrom, &c.Note, &c.CreatedByName, &c.CreatedAt) == nil {
			out = append(out, c)
		}
	}
	return out
}

// liveMonthSummary 按最新打卡实时重算某人某月（仅供未结算月计算 / 已结算月复核对照）
func (s *server) liveMonthSummary(wk *Worker, month string) *liveSummary {
	from, to := monthRange(month)
	dates := eachDate(from, to)
	punches, err := s.loadPunches(wk.SiteID, []int64{wk.ID}, from, to)
	if err != nil {
		return nil
	}
	pa := s.buildAttendance(wk, dates, punches[wk.ID], map[int64]eligibility{})
	c, err := s.activeContract(wk.ID, month)
	if err != nil || c == nil {
		return &liveSummary{Workdays: pa.Workdays, Hours: pa.Hours, Amount: 0}
	}
	return &liveSummary{
		Workdays:   pa.Workdays,
		Hours:      pa.Hours,
		RateVer:    c.Version,
		DayRate:    c.DayRate,
		Amount:     round2(pa.Workdays * c.DayRate),
		ContractID: c.ID,
	}
}

func money(v float64) float64 { return round2(v) }

// payrollVisibleWorkers 分账可见工人（与出勤同口径隔离）
func (s *server) payrollWorkers(u *User, siteID int64) ([]Worker, *apiError) {
	team := ""
	if u.Role == RoleSubLeader {
		team = u.Team
	}
	return s.scopeWorkers(u, siteID, team)
}

func canManagePayroll(u *User) bool { return u.Role == RoleGCAdmin }

// ---------- 月分账详情 ----------

func (s *server) handlePayroll(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query()
	siteID, aerr := s.resolveAttendanceSite(u, q)
	if aerr != nil {
		aerr.write(w)
		return
	}
	month := q.Get("month")
	if month == "" {
		month = time.Now().In(cst).Format("2006-01")
	}
	if len(month) != 7 {
		writeErr(w, http.StatusBadRequest, "月份格式应为 YYYY-MM")
		return
	}
	workers, aerr := s.payrollWorkers(u, siteID)
	if aerr != nil {
		aerr.write(w)
		return
	}

	var pmID int64
	var status, settledAt, settledByName string
	err := s.db.QueryRow(`SELECT id, status, COALESCE(settled_at,''), COALESCE(settled_by_name,'')
		FROM payroll_months WHERE site_id = ? AND month = ?`, siteID, month).
		Scan(&pmID, &status, &settledAt, &settledByName)
	settled := err == nil && status == "settled"

	items := []map[string]any{}
	var totalAmount, totalPaid, totalAdj, totalCurrent float64
	dirtyCnt := 0

	if settled {
		rows, err := s.db.Query(`
			SELECT worker_id, rate_version, day_rate, work_days, work_hours, amount, dirty, current_amount
			FROM payroll_items WHERE payroll_month_id = ? ORDER BY worker_id`, pmID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查询分账失败")
			return
		}
		type rawItem struct {
			wid                            int64
			rv                             int
			rate, days, hours, amount, cur float64
			dirty                          int
		}
		raws := []rawItem{}
		for rows.Next() {
			var x rawItem
			if rows.Scan(&x.wid, &x.rv, &x.rate, &x.days, &x.hours, &x.amount, &x.dirty, &x.cur) == nil {
				raws = append(raws, x)
			}
		}
		rows.Close()
		workerMap := map[int64]Worker{}
		for _, wk := range workers {
			workerMap[wk.ID] = wk
		}
		// 注意：SQLite 单连接，必须先关闭 Rows 再做关联查询，否则自等锁死
		for _, x := range raws {
			wk, ok := workerMap[x.wid]
			if !ok {
				continue // 已不在数据范围内（如跨工地），不展示
			}
			paid, adj := s.paidAndAdjusted(pmID, x.wid)
			final := money(x.amount + adj)
			pending := mathMax(0, money(final-paid))
			if x.dirty == 1 {
				dirtyCnt++
			}
			totalAmount += x.amount
			totalPaid += paid
			totalAdj += adj
			totalCurrent += x.cur
			// 工人查看本人分账时显示全名
			displayName := maskName(wk.FullName)
			if u.Role == RoleWorker && u.WorkerID != nil && *u.WorkerID == wk.ID {
				displayName = wk.FullName
			}
			items = append(items, map[string]any{
				"worker_id": wk.ID, "job_no": wk.JobNo, "name": displayName, "team": wk.Team, "trade": wk.Trade,
				"frozen": true, "rate_version": x.rv, "day_rate": x.rate,
				"work_days": x.days, "work_hours": x.hours, "amount": x.amount,
				"adjusted": adj != 0, "adjustment": adj, "final_amount": final,
				"paid_amount": paid, "pending_amount": pending,
				"overpaid": final < paid,
				"dirty":    x.dirty == 1, "current_amount": x.cur,
				"current_diff": money(x.cur - x.amount),
				"contracts":    s.listContracts(wk.ID),
			})
		}
	} else {
		// 未结算：实时算，单价用当月适用版本；历史版本变更不影响（新版本只对生效月起作用）
		for i := range workers {
			wk := &workers[i]
			cur := s.liveMonthSummary(wk, month)
			if cur.Workdays == 0 {
				continue // 当月无工日，不进入待结算清单
			}
			totalAmount += cur.Amount
			// 工人查看本人分账时显示全名
			displayName := maskName(wk.FullName)
			if u.Role == RoleWorker && u.WorkerID != nil && *u.WorkerID == wk.ID {
				displayName = wk.FullName
			}
			items = append(items, map[string]any{
				"worker_id": wk.ID, "job_no": wk.JobNo, "name": displayName, "team": wk.Team, "trade": wk.Trade,
				"frozen": false, "rate_version": cur.RateVer, "day_rate": cur.DayRate,
				"work_days": cur.Workdays, "work_hours": cur.Hours, "amount": cur.Amount,
				"adjusted": false, "adjustment": 0, "final_amount": cur.Amount,
				"paid_amount": 0, "pending_amount": cur.Amount,
				"dirty": false, "current_amount": cur.Amount, "current_diff": 0,
				"contracts": s.listContracts(wk.ID),
			})
		}
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i]["team"].(string) < items[j]["team"].(string) || items[i]["job_no"].(string) < items[j]["job_no"].(string)
	})

	// 月清单（选择器）
	months := s.listPayrollMonths(siteID)

	writeJSON(w, http.StatusOK, map[string]any{
		"site_id": siteID, "month": month,
		"status":     map[bool]string{true: "settled", false: "open"}[settled],
		"settled_at": settledAt, "settled_by_name": settledByName,
		"can_manage": canManagePayroll(u),
		"items":      items,
		"months":     months,
		"totals": map[string]any{
			"frozen_amount":  money(totalAmount),
			"adjustment":     money(totalAdj),
			"final_amount":   money(totalAmount + totalAdj),
			"paid_amount":    money(totalPaid),
			"pending_amount": money(mathMax(0, totalAmount+totalAdj-totalPaid)),
			"current_amount": money(totalCurrent),
			"dirty_count":    dirtyCnt,
			"people":         len(items),
		},
		"calibers":    []map[string]string{caliberMeta[CaliberDays], caliberMeta[CaliberHours]},
		"server_time": nowUTC(),
	})
}

func mathMax(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func (s *server) paidAndAdjusted(pmID, workerID int64) (float64, float64) {
	var paid, adj sql.NullFloat64
	s.db.QueryRow(`SELECT COALESCE(SUM(amount),0) FROM payroll_payments WHERE payroll_month_id=? AND worker_id=?`, pmID, workerID).Scan(&paid)
	s.db.QueryRow(`SELECT COALESCE(SUM(delta),0) FROM payroll_adjustments WHERE payroll_month_id=? AND worker_id=?`, pmID, workerID).Scan(&adj)
	return paid.Float64, adj.Float64
}

func (s *server) listPayrollMonths(siteID int64) []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	cur := time.Now().In(cst).Format("2006-01")
	rows, err := s.db.Query(`
		SELECT pm.month, pm.status, COALESCE(pm.settled_at,''), COALESCE(pm.settled_by_name,''),
		       (SELECT COUNT(*) FROM payroll_items pi WHERE pi.payroll_month_id=pm.id AND pi.dirty=1)
		FROM payroll_months pm WHERE pm.site_id = ? ORDER BY pm.month DESC`, siteID)
	if err == nil {
		for rows.Next() {
			var m, st, sa, sn string
			var dirty int
			if rows.Scan(&m, &st, &sa, &sn, &dirty) == nil {
				out = append(out, map[string]any{"month": m, "status": st, "settled_at": sa, "settled_by_name": sn, "dirty_count": dirty, "has_payment": false})
				seen[m] = true
			}
		}
		rows.Close()
	}
	// 有打卡但还没建账单的月份（外层 Rows 必须先关闭，单连接不能嵌套查询）
	prows, perr := s.db.Query(`SELECT DISTINCT substr(punch_date,1,7) FROM attendance_punches WHERE site_id=? ORDER BY 1 DESC`, siteID)
	if perr == nil {
		for prows.Next() {
			var m string
			if prows.Scan(&m) == nil && !seen[m] {
				out = append(out, map[string]any{"month": m, "status": "open", "dirty_count": 0, "has_payment": false})
				seen[m] = true
			}
		}
		prows.Close()
	}
	if !seen[cur] {
		out = append(out, map[string]any{"month": cur, "status": "open", "dirty_count": 0, "has_payment": false})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["month"].(string) > out[j]["month"].(string) })
	return out
}

// ---------- 结算（冻结快照） ----------

type settleReq struct {
	Month      string `json:"month"`
	Confirm    bool   `json:"confirm"`
	ClientOpID string `json:"client_op_id"`
}

func (s *server) handleSettle(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !canManagePayroll(u) {
		writeErr(w, http.StatusForbidden, "仅总包管理员可执行月结算")
		return
	}
	var req settleReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(req.Month) != 7 {
		writeErr(w, http.StatusBadRequest, "月份格式应为 YYYY-MM")
		return
	}
	siteID := *u.SiteID
	var existing string
	err := s.db.QueryRow(`SELECT status FROM payroll_months WHERE site_id=? AND month=?`, siteID, req.Month).Scan(&existing)
	if err == nil && existing == "settled" {
		writeErr(w, http.StatusConflict, req.Month+" 月已结算冻结，不能重复结算；打卡更正请在分账中复核补差")
		return
	}

	workers, aerr := s.payrollWorkers(u, siteID)
	if aerr != nil {
		aerr.write(w)
		return
	}
	type snap struct {
		wk                *Worker
		days, hours, rate float64
		ver               int
		amount            float64
	}
	snaps := []snap{}
	var total float64
	for i := range workers {
		wk := &workers[i]
		cur := s.liveMonthSummary(wk, req.Month)
		if cur.Workdays <= 0 {
			continue
		}
		snaps = append(snaps, snap{wk, cur.Workdays, cur.Hours, cur.DayRate, cur.RateVer, cur.Amount})
		total += cur.Amount
	}
	if len(snaps) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, req.Month+" 月没有可结算的工日（无人有有效打卡），无法结算")
		return
	}
	if !req.Confirm {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        "结算后单价、工日、金额将按当前版本冻结，之后合同调价不影响本月；请二次确认",
			"need_confirm": true,
			"month":        req.Month,
			"people":       len(snaps),
			"total":        money(total),
		})
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()
	var pmID int64
	if existing == "open" {
		tx.Exec(`UPDATE payroll_months SET status='settled', settled_at=?, settled_by=?, settled_by_name=? WHERE site_id=? AND month=?`,
			nowUTC(), u.ID, u.Name, siteID, req.Month)
		tx.QueryRow(`SELECT id FROM payroll_months WHERE site_id=? AND month=?`, siteID, req.Month).Scan(&pmID)
	} else {
		res, err := tx.Exec(`INSERT INTO payroll_months(site_id, month, status, settled_at, settled_by, settled_by_name, created_at)
			VALUES(?,?, 'settled',?,?,?,?)`,
			siteID, req.Month, nowUTC(), u.ID, u.Name, nowUTC())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
			return
		}
		pmID, _ = res.LastInsertId()
	}
	for _, sp := range snaps {
		if _, err := tx.Exec(`
			INSERT INTO payroll_items(payroll_month_id, worker_id, rate_version, day_rate, work_days, work_hours, amount, current_amount)
			VALUES(?,?,?,?,?,?,?,?)`,
			pmID, sp.wk.ID, sp.ver, sp.rate, sp.days, sp.hours, sp.amount, sp.amount); err != nil {
			writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "month": req.Month, "payroll_month_id": pmID,
		"people": len(snaps), "total": money(total),
		"message": req.Month + " 月已结算冻结：共" + strconv.Itoa(len(snaps)) + "人，合计¥" + fmt.Sprintf("%.2f", money(total)) +
			"。此后合同调价与打卡更正均不改变本快照，差额走复核补差。",
	})
}

// ---------- 发薪（独立流水，支持部分发放，绝不被冲销） ----------

type payReq struct {
	Month   string `json:"month"`
	Workers []struct {
		WorkerID int64   `json:"worker_id"`
		Amount   float64 `json:"amount"`
	} `json:"workers"`
	Note string `json:"note"`
}

func (s *server) handlePay(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !canManagePayroll(u) {
		writeErr(w, http.StatusForbidden, "仅总包管理员可登记发放")
		return
	}
	var req payReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	siteID := *u.SiteID
	var pmID int64
	var status string
	if err := s.db.QueryRow(`SELECT id, status FROM payroll_months WHERE site_id=? AND month=?`, siteID, req.Month).Scan(&pmID, &status); err != nil || status != "settled" {
		writeErr(w, http.StatusConflict, "只有已结算月份可以登记发放；请先结算 "+req.Month+" 月")
		return
	}
	if len(req.Workers) == 0 {
		writeErr(w, http.StatusBadRequest, "请选择发放人员与金额")
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()
	paid := 0
	var sum float64
	for _, it := range req.Workers {
		if it.Amount <= 0 {
			continue
		}
		// 只能给本工地快照内的人发钱
		var frozen float64
		if err := tx.QueryRow(`SELECT amount FROM payroll_items WHERE payroll_month_id=? AND worker_id=?`, pmID, it.WorkerID).Scan(&frozen); err != nil {
			writeErr(w, http.StatusBadRequest, "存在不在本月结算清单内的人员，已取消")
			return
		}
		if _, err := tx.Exec(`INSERT INTO payroll_payments(payroll_month_id, worker_id, amount, note, paid_by, paid_by_name, created_at)
			VALUES(?,?,?,?,?,?,?)`, pmID, it.WorkerID, money(it.Amount), strings.TrimSpace(req.Note), u.ID, u.Name, nowUTC()); err != nil {
			writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
			return
		}
		paid++
		sum += it.Amount
	}
	if paid == 0 {
		writeErr(w, http.StatusBadRequest, "发放金额必须大于0")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "paid_people": paid, "paid_total": money(sum),
		"message": fmt.Sprintf("已登记发放%d人、¥%.2f。已发部分独立留底，后续重算或复核均不会冲销。", paid, money(sum)),
	})
}

// ---------- 复核：结算后打卡差异 ----------

type reviewReq struct {
	Month    string `json:"month"`
	WorkerID int64  `json:"worker_id"`
	Action   string `json:"action"` // apply=按最新口径补差 / keep=维持原金额
	Reason   string `json:"reason"`
}

func (s *server) handleReview(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !canManagePayroll(u) {
		writeErr(w, http.StatusForbidden, "仅总包管理员可复核调整")
		return
	}
	var req reviewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len([]rune(req.Reason)) < 2 {
		writeErr(w, http.StatusBadRequest, "结算已冻结，复核调整必须填写原因（不少于2个字），将留痕")
		return
	}
	siteID := *u.SiteID
	var pmID int64
	if err := s.db.QueryRow(`SELECT id FROM payroll_months WHERE site_id=? AND month=? AND status='settled'`, siteID, req.Month).Scan(&pmID); err != nil {
		writeErr(w, http.StatusNotFound, req.Month+" 月未结算或不存在")
		return
	}
	var frozen, current float64
	var dirty int
	if err := s.db.QueryRow(`SELECT amount, current_amount, dirty FROM payroll_items WHERE payroll_month_id=? AND worker_id=?`, pmID, req.WorkerID).Scan(&frozen, &current, &dirty); err != nil {
		writeErr(w, http.StatusNotFound, "该人员不在本月结算清单内")
		return
	}
	if dirty != 1 {
		writeErr(w, http.StatusConflict, "该人员本月没有待复核的打卡差异")
		return
	}
	// 以实时重算为准（防止复核时数据又有变化）
	wk, err := s.getWorker(req.WorkerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	live := s.liveMonthSummary(wk, req.Month)
	paid, priorAdj := s.paidAndAdjusted(pmID, req.WorkerID)
	delta := 0.0
	if req.Action == "apply" {
		delta = money(live.Amount - frozen - priorAdj)
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO payroll_adjustments(payroll_month_id, worker_id, delta, reason, actor_id, actor_name, created_at)
		VALUES(?,?,?,?,?,?,?)`, pmID, req.WorkerID, delta, req.Reason, u.ID, u.Name, nowUTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	if _, err := tx.Exec(`UPDATE payroll_items SET dirty=0, current_amount=? WHERE payroll_month_id=? AND worker_id=?`, live.Amount, pmID, req.WorkerID); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	final := money(frozen + priorAdj + delta)
	pending := mathMax(0, money(final-paid))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "delta": delta, "final_amount": final, "paid_amount": paid, "pending_amount": pending,
		"message": fmt.Sprintf("复核完成：%s 补差¥%.2f，冻结原金额¥%.2f不变，已发¥%.2f不冲销，剩余待发¥%.2f。",
			req.Month, delta, frozen, paid, pending),
	})
}

// ---------- 合同新版本 ----------

type contractReq struct {
	WorkerID      int64   `json:"worker_id"`
	DayRate       float64 `json:"day_rate"`
	EffectiveFrom string  `json:"effective_from"` // YYYY-MM
	Note          string  `json:"note"`
}

func (s *server) handleCreateContract(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !canManagePayroll(u) {
		writeErr(w, http.StatusForbidden, "仅总包管理员可调整合同单价")
		return
	}
	var req contractReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.DayRate <= 0 {
		writeErr(w, http.StatusBadRequest, "日单价必须大于0")
		return
	}
	if len(req.EffectiveFrom) != 7 {
		writeErr(w, http.StatusBadRequest, "生效月份格式应为 YYYY-MM")
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
	if *u.SiteID != wk.SiteID {
		writeErr(w, http.StatusForbidden, "无权为其他工地人员调整合同")
		return
	}
	var maxVer int
	s.db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM worker_contracts WHERE worker_id=?`, req.WorkerID).Scan(&maxVer)
	// 已结算月份若早新生效月不受影响；若新生效月 <= 已结算月，明确拦截（保护冻结语义）
	var frozenMonths int
	s.db.QueryRow(`SELECT COUNT(*) FROM payroll_months pm
		WHERE pm.site_id=? AND pm.status='settled' AND pm.month >= ?`, wk.SiteID, req.EffectiveFrom).Scan(&frozenMonths)
	if frozenMonths > 0 {
		writeErr(w, http.StatusConflict, fmt.Sprintf(
			"生效月份 %s 及之后已有 %d 个结算月冻结在原单价，新版本不能追溯改价；请把生效月设在未结算月份", req.EffectiveFrom, frozenMonths))
		return
	}
	res, err := s.db.Exec(`INSERT INTO worker_contracts(worker_id, version, day_rate, effective_from, note, created_by, created_by_name, created_at)
		VALUES(?,?,?,?,?,?,?,?)`,
		req.WorkerID, maxVer+1, money(req.DayRate), req.EffectiveFrom, strings.TrimSpace(req.Note), u.ID, u.Name, nowUTC())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败，请重试")
		return
	}
	id, _ := res.LastInsertId()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "version": maxVer + 1,
		"message": fmt.Sprintf("合同v%d已建立：¥%.2f/工日，%s起生效；历史已结算月仍冻结在原单价版本", maxVer+1, money(req.DayRate), req.EffectiveFrom),
	})
}

func (s *server) handleListContracts(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	wid, err := strconv.ParseInt(r.URL.Query().Get("worker_id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少 worker_id")
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
	writeJSON(w, http.StatusOK, map[string]any{"contracts": s.listContracts(wid)})
}

// ---------- 分账导出 CSV ----------

func (s *server) handlePayrollExport(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	q := r.URL.Query()
	siteID, aerr := s.resolveAttendanceSite(u, q)
	if aerr != nil {
		aerr.write(w)
		return
	}
	month := q.Get("month")
	if month == "" {
		month = time.Now().In(cst).Format("2006-01")
	}
	// 复用 handlePayroll 的数据太绕，直接查一遍快照/实时
	var pmID int64
	var status string
	s.db.QueryRow(`SELECT id, status FROM payroll_months WHERE site_id=? AND month=?`, siteID, month).Scan(&pmID, &status)
	workers, _ := s.payrollWorkers(u, siteID)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="payroll_%s.csv"`, month))
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	cw.Write([]string{"# 工资分账（" + month + "，" + map[string]string{"settled": "已结算冻结", "open": "未结算（实时计算）"}[status] + "）"})
	cw.Write([]string{"# 工日口径：出勤工日 = 有效打卡工时 ÷ 8（半天班0.5工日）；已发金额独立留底，不随重算冲销"})
	cw.Write([]string{"工号", "姓名（脱敏）", "班组", "工种", "单价版本", "日单价", "出勤工日", "有效工时", "冻结金额", "复核补差", "应发合计", "已发金额", "待发金额", "状态"})

	row := func(wk *Worker, ver int, rate, days, hours, amount, current float64, frozen bool, dirty bool) {
		paid, adj := 0.0, 0.0
		if frozen {
			paid, adj = s.paidAndAdjusted(pmID, wk.ID)
		}
		final := money(amount + adj)
		st := "未结算"
		if frozen {
			st = "已冻结"
			if dirty {
				st = "有差异待复核"
			} else if paid > final {
				st = "已超额发放"
			} else if paid >= final && final > 0 {
				st = "已发清"
			} else if paid > 0 {
				st = "部分发放"
			}
		}
		cw.Write([]string{
			wk.JobNo, maskName(wk.FullName), wk.Team, wk.Trade,
			"v" + strconv.Itoa(ver), fmt.Sprintf("%.2f", rate),
			strconv.FormatFloat(days, 'f', 2, 64), strconv.FormatFloat(hours, 'f', 0, 64),
			fmt.Sprintf("%.2f", amount), fmt.Sprintf("%.2f", adj), fmt.Sprintf("%.2f", final),
			fmt.Sprintf("%.2f", paid), fmt.Sprintf("%.2f", mathMax(0, final-paid)), st,
		})
	}
	if status == "settled" {
		type exportRow struct {
			wid                                int64
			ver                                int
			rate, days, hours, amount, current float64
			dirty                              int
		}
		rows, err := s.db.Query(`SELECT worker_id, rate_version, day_rate, work_days, work_hours, amount, dirty, current_amount
			FROM payroll_items WHERE payroll_month_id=? ORDER BY worker_id`, pmID)
		raws := []exportRow{}
		if err == nil {
			for rows.Next() {
				var x exportRow
				if rows.Scan(&x.wid, &x.ver, &x.rate, &x.days, &x.hours, &x.amount, &x.dirty, &x.current) == nil {
					raws = append(raws, x)
				}
			}
			rows.Close()
		}
		wm := map[int64]Worker{}
		for _, wk := range workers {
			wm[wk.ID] = wk
		}
		for _, x := range raws {
			if wk, ok := wm[x.wid]; ok {
				row(&wk, x.ver, x.rate, x.days, x.hours, x.amount, x.current, true, x.dirty == 1)
			}
		}
	} else {
		for i := range workers {
			wk := &workers[i]
			cur := s.liveMonthSummary(wk, month)
			if cur.Workdays > 0 {
				row(wk, cur.RateVer, cur.DayRate, cur.Workdays, cur.Hours, cur.Amount, cur.Amount, false, false)
			}
		}
	}
	cw.Flush()
}

// guard: 保留 encoding/json 引用（部分编译环境下文件内只间接使用）
var _ = json.Marshal
