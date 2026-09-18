package main

import (
	"net/http"
	"net/url"
	"strconv"
)

// handleListSites 当前用户可见的工地列表（监管员全部，其余本人工地）
func (s *server) handleListSites(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	sites := []Site{}
	if u.Role == RoleRegulator {
		rows, err := s.db.Query(`SELECT id, code, name, address, gc_company FROM sites ORDER BY id`)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查询失败")
			return
		}
		defer rows.Close()
		for rows.Next() {
			var st Site
			if rows.Scan(&st.ID, &st.Code, &st.Name, &st.Address, &st.GCCompany) == nil {
				sites = append(sites, st)
			}
		}
	} else {
		if u.SiteID == nil {
			writeErr(w, http.StatusForbidden, "账号未绑定工地")
			return
		}
		var st Site
		err := s.db.QueryRow(`SELECT id, code, name, address, gc_company FROM sites WHERE id = ?`, *u.SiteID).
			Scan(&st.ID, &st.Code, &st.Name, &st.Address, &st.GCCompany)
		if err != nil {
			writeErr(w, http.StatusNotFound, "工地不存在")
			return
		}
		sites = append(sites, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites, "server_time": nowUTC()})
}

// resolveSiteID 校验用户是否有权查看目标工地，返回最终 site_id
func (s *server) resolveSiteID(u *User, r *http.Request) (int64, *apiError) {
	param := r.URL.Query().Get("site_id")
	if u.Role != RoleRegulator {
		if u.SiteID == nil {
			return 0, &apiError{HTTPCode: 403, Msg: "账号未绑定工地"}
		}
		if param != "" {
			if id, err := strconv.ParseInt(param, 10, 64); err == nil && id != *u.SiteID {
				return 0, &apiError{HTTPCode: 403, Msg: "无权查看其他工地的作战台"}
			}
		}
		return *u.SiteID, nil
	}
	// 监管员：默认第一个工地
	if param != "" {
		if id, err := strconv.ParseInt(param, 10, 64); err == nil {
			return id, nil
		}
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM sites ORDER BY id LIMIT 1`).Scan(&id); err != nil {
		return 0, &apiError{HTTPCode: 404, Msg: "暂无工地数据"}
	}
	return id, nil
}

// handleDashboard 作战台聚合：在场漏斗 + 今日应培训 + 隐患 + 告警红点
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	siteID, aerr := s.resolveSiteID(u, r)
	if aerr != nil {
		aerr.write(w)
		return
	}
	var site Site
	if err := s.db.QueryRow(`SELECT id, code, name, address, gc_company FROM sites WHERE id = ?`, siteID).
		Scan(&site.ID, &site.Code, &site.Name, &site.Address, &site.GCCompany); err != nil {
		writeErr(w, http.StatusNotFound, "工地不存在")
		return
	}

	// 在场漏斗：各状态人数
	funnel := map[string]int{
		"registered": 0, "pending": 0, "onsite": 0, "leave": 0, "exited": 0, "blacklisted": 0,
	}
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM workers WHERE site_id = ? GROUP BY status`, siteID)
	if err == nil {
		for rows.Next() {
			var st string
			var n int
			if rows.Scan(&st, &n) == nil {
				funnel[st] = n
				funnel["registered"] += n
			}
		}
		rows.Close()
	}

	// 今日应培训
	trainings := []map[string]any{}
	trows, err := s.db.Query(`
		SELECT t.id, t.title, t.due_date, t.status, w.id, w.full_name, w.job_no, w.team
		FROM trainings t JOIN workers w ON w.id = t.worker_id
		WHERE t.site_id = ? AND t.due_date <= ? AND t.status = 'pending'
		ORDER BY t.due_date, w.job_no`, siteID, todayLocal())
	if err == nil {
		for trows.Next() {
			var tid int64
			var title, due, status, fullName, jobNo, team string
			var wid int64
			if trows.Scan(&tid, &title, &due, &status, &wid, &fullName, &jobNo, &team) == nil {
				trainings = append(trainings, map[string]any{
					"id": tid, "title": title, "due_date": due, "status": status,
					"worker_id": wid, "worker_label": workerLabel(fullName, jobNo), "team": team,
				})
			}
		}
		trows.Close()
	}

	// 隐患（未闭环）
	hazards := []map[string]any{}
	hrows, err := s.db.Query(`
		SELECT id, title, level, status, created_at FROM hazards
		WHERE site_id = ? AND status != 'closed'
		ORDER BY CASE level WHEN '重大' THEN 0 WHEN '较大' THEN 1 ELSE 2 END, id DESC`, siteID)
	if err == nil {
		for hrows.Next() {
			var id int64
			var title, level, status, createdAt string
			if hrows.Scan(&id, &title, &level, &status, &createdAt) == nil {
				hazards = append(hazards, map[string]any{
					"id": id, "title": title, "level": level, "status": status, "created_at": createdAt,
				})
			}
		}
		hrows.Close()
	}

	// 告警（红点）
	alerts := []map[string]any{}
	unread := 0
	arows, err := s.db.Query(`
		SELECT id, type, message, level, read, created_at FROM alerts
		WHERE site_id = ? ORDER BY id DESC LIMIT 20`, siteID)
	if err == nil {
		for arows.Next() {
			var id int64
			var typ, msg, level, createdAt string
			var read int
			if arows.Scan(&id, &typ, &msg, &level, &read, &createdAt) == nil {
				if read == 0 {
					unread++
				}
				alerts = append(alerts, map[string]any{
					"id": id, "type": typ, "message": msg, "level": level,
					"read": read == 1, "created_at": createdAt,
				})
			}
		}
		arows.Close()
	}

	// 出勤打卡速览（本周双口径 + 今日判定计数；口径文案与出勤详情/导出一致）
	attBrief := s.buildAttendanceBrief(u, r, siteID)

	writeJSON(w, http.StatusOK, map[string]any{
		"site":            site,
		"funnel":          funnel,
		"today_trainings": trainings,
		"hazards":         hazards,
		"open_hazards":    len(hazards),
		"alerts":          alerts,
		"unread_alerts":   unread,
		"attendance":      attBrief,
		"server_time":     nowUTC(),
	})
}

// buildAttendanceBrief 作战台出勤卡：本周甘特双口径 + 今日出勤/缺勤/迟到/早退计数
func (s *server) buildAttendanceBrief(u *User, r *http.Request, siteID int64) map[string]any {
	sc := &attScope{siteID: siteID}
	switch u.Role {
	case RoleRegulator:
		sc.readOnly = true
	case RoleSubLeader:
		sc.team = u.Team
	case RoleWorker:
		return map[string]any{"hidden": true}
	}
	from, to, _ := ganttRange(url.Values{})
	out, aerr := s.buildGantt(sc, from, to)
	if aerr != nil {
		return map[string]any{"error": aerr.Msg}
	}
	today := todayLocal()
	counts := map[string]int{"present": 0, "late": 0, "early": 0, "absent": 0, "rest": 0, "void": 0, "no_record": 0}
	punched := 0
	eligibleWorkers := 0
	for _, wm := range out["workers"].([]map[string]any) {
		// 与出勤率口径一致：仅在场或本周确有打卡者计入今日判定，请假/退场/黑名单/待入场不算缺勤
		if wm["worker_status"] != string(StatusOnsite) && !wm["has_any_punch"].(bool) {
			continue
		}
		eligibleWorkers++
		c, ok := wm["days"].(map[string]*dayCell)[today]
		if !ok {
			counts["no_record"]++
			continue
		}
		if c.Total > 0 {
			punched++
		}
		if _, exists := counts[c.Status]; exists {
			counts[c.Status]++
		}
	}
	total := out["total"].(map[string]any)
	return map[string]any{
		"week_from": out["from"], "week_to": out["to"],
		"today_counts": counts, "punched_today": punched,
		"eligible_today":     eligibleWorkers,
		"rate_days":          total["rate_days"],
		"rate_hours":         total["rate_hours"],
		"scheduled_days":     total["scheduled_days"],
		"present_days":       total["present_days"],
		"worked_minutes":     total["worked_minutes"],
		"scheduled_minutes":  total["scheduled_minutes"],
		"caliber_days_text":  caliberDaysText,
		"caliber_hours_text": caliberHoursText,
		"half_day_teams":     out["rule"].(map[string]any)["half_day_teams"],
	}
}

// handleReadAlert 单条告警已读（消红点）
func (s *server) handleReadAlert(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == RoleWorker {
		writeErr(w, http.StatusForbidden, "工人账号无权操作告警")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "告警编号格式错误")
		return
	}
	if u.Role != RoleRegulator {
		res, err := s.db.Exec(`UPDATE alerts SET read = 1 WHERE id = ? AND site_id = ?`, id, *u.SiteID)
		_ = res
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "操作失败")
			return
		}
	} else {
		s.db.Exec(`UPDATE alerts SET read = 1 WHERE id = ?`, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleReadAllAlerts 一键消红点
func (s *server) handleReadAllAlerts(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == RoleWorker {
		writeErr(w, http.StatusForbidden, "工人账号无权操作告警")
		return
	}
	siteID, aerr := s.resolveSiteID(u, r)
	if aerr != nil {
		aerr.write(w)
		return
	}
	s.db.Exec(`UPDATE alerts SET read = 1 WHERE site_id = ?`, siteID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
