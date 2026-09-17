package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// ---------- 查询辅助 ----------

func (s *server) getWorker(id int64) (*Worker, error) {
	w := &Worker{}
	var face, idv int
	err := s.db.QueryRow(`
		SELECT id, site_id, job_no, full_name, id_card, phone, team, trade, status,
		       face_verified, id_verified, version, created_at, updated_at
		FROM workers WHERE id = ?`, id).
		Scan(&w.ID, &w.SiteID, &w.JobNo, &w.FullName, &w.IDCard, &w.Phone, &w.Team, &w.Trade,
			&w.Status, &face, &idv, &w.Version, &w.CreatedAt, &w.UpdatedAt)
	if err != nil {
		return nil, err
	}
	w.FaceVerified = face == 1
	w.IDVerified = idv == 1
	return w, nil
}

// canViewWorker 数据隔离：工人只能看自己；班组长限本班组；总包/监理限本工地；监管员全量只读
func canViewWorker(u *User, w *Worker) *apiError {
	switch u.Role {
	case RoleRegulator:
		return nil
	case RoleGCAdmin, RoleSupervisor:
		if u.SiteID != nil && *u.SiteID == w.SiteID {
			return nil
		}
		return &apiError{HTTPCode: 403, Msg: "无权查看其他工地的人员档案"}
	case RoleSubLeader:
		if u.SiteID != nil && *u.SiteID == w.SiteID && u.Team == w.Team {
			return nil
		}
		return &apiError{HTTPCode: 403, Msg: "班组长只能查看本班组人员档案"}
	case RoleWorker:
		if u.WorkerID != nil && *u.WorkerID == w.ID {
			return nil
		}
		return &apiError{HTTPCode: 403, Msg: "无权查看他人档案，仅可查看本人档案"}
	}
	return &apiError{HTTPCode: 403, Msg: "无权查看该档案"}
}

// workerSummary 列表/看板通用脱敏视图：缩写 + 工号
func workerSummary(w *Worker) map[string]any {
	return map[string]any{
		"id":           w.ID,
		"site_id":      w.SiteID,
		"job_no":       w.JobNo,
		"name":         maskName(w.FullName),
		"name_masked":  true,
		"label":        workerLabel(w.FullName, w.JobNo),
		"team":         w.Team,
		"trade":        w.Trade,
		"status":       w.Status,
		"status_label": statusLabel(w.Status),
		"updated_at":   w.UpdatedAt,
	}
}

// ---------- handlers ----------

func (s *server) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == RoleWorker {
		writeErr(w, http.StatusForbidden, "工人账号无权浏览人员列表，请查看本人档案")
		return
	}
	q := r.URL.Query()
	var args []any
	var conds []string

	// 数据范围
	switch u.Role {
	case RoleRegulator:
		if v := q.Get("site_id"); v != "" {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil {
				conds = append(conds, "site_id = ?")
				args = append(args, id)
			}
		}
	case RoleGCAdmin, RoleSupervisor:
		if u.SiteID == nil {
			writeErr(w, http.StatusForbidden, "账号未绑定工地")
			return
		}
		conds = append(conds, "site_id = ?")
		args = append(args, *u.SiteID)
	case RoleSubLeader:
		if u.SiteID == nil {
			writeErr(w, http.StatusForbidden, "账号未绑定工地")
			return
		}
		conds = append(conds, "site_id = ?", "team = ?")
		args = append(args, *u.SiteID, u.Team)
	}
	if st := q.Get("status"); st != "" {
		if _, ok := statusLabels[WorkerStatus(st)]; !ok {
			writeErr(w, http.StatusBadRequest, "未知的状态筛选值")
			return
		}
		conds = append(conds, "status = ?")
		args = append(args, st)
	}
	if kw := strings.TrimSpace(q.Get("q")); kw != "" {
		conds = append(conds, "(job_no LIKE ? OR trade LIKE ? OR team LIKE ?)")
		like := "%" + kw + "%"
		args = append(args, like, like, like)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := s.db.Query(`
		SELECT id, site_id, job_no, full_name, team, trade, status, updated_at
		FROM workers `+where+` ORDER BY status, job_no`, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	defer rows.Close()
	list := []map[string]any{}
	for rows.Next() {
		wk := &Worker{}
		if err := rows.Scan(&wk.ID, &wk.SiteID, &wk.JobNo, &wk.FullName, &wk.Team, &wk.Trade, &wk.Status, &wk.UpdatedAt); err != nil {
			writeErr(w, http.StatusInternalServerError, "查询失败")
			return
		}
		list = append(list, workerSummary(wk))
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": list, "server_time": nowUTC()})
}

// handleMyWorker 工人查看本人档案
func (s *server) handleMyWorker(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role != RoleWorker || u.WorkerID == nil {
		writeErr(w, http.StatusBadRequest, "当前账号不是工人账号")
		return
	}
	s.serveWorkerDetail(w, r, *u.WorkerID)
}

func (s *server) handleGetWorker(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "档案编号格式错误")
		return
	}
	s.serveWorkerDetail(w, r, id)
}

func (s *server) serveWorkerDetail(w http.ResponseWriter, r *http.Request, id int64) {
	u := currentUser(r)
	wk, err := s.getWorker(id)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "档案不存在或已被删除")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	if aerr := canViewWorker(u, wk); aerr != nil {
		aerr.write(w)
		return
	}
	self := u.Role == RoleWorker && u.WorkerID != nil && *u.WorkerID == wk.ID

	// 状态时间线
	events := []map[string]any{}
	rows, err := s.db.Query(`
		SELECT id, from_status, to_status, actor_name, actor_role, reason, created_at
		FROM status_events WHERE worker_id = ? ORDER BY id DESC`, wk.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var eid int64
			var from, to, actorName, actorRole, reason, createdAt string
			if rows.Scan(&eid, &from, &to, &actorName, &actorRole, &reason, &createdAt) == nil {
				events = append(events, map[string]any{
					"id": eid, "from_status": from, "from_label": statusLabel(from),
					"to_status": to, "to_label": statusLabel(to),
					"actor_name": actorName, "actor_role": actorRole, "actor_role_label": roleLabels[actorRole],
					"reason": reason, "created_at": createdAt,
				})
			}
		}
	}

	detail := map[string]any{
		"id":            wk.ID,
		"site_id":       wk.SiteID,
		"job_no":        wk.JobNo,
		"team":          wk.Team,
		"trade":         wk.Trade,
		"status":        wk.Status,
		"status_label":  statusLabel(wk.Status),
		"face_verified": wk.FaceVerified,
		"id_verified":   wk.IDVerified,
		"version":       wk.Version,
		"created_at":    wk.CreatedAt,
		"updated_at":    wk.UpdatedAt,
		"id_card":       maskIDCard(wk.IDCard),
		"phone":         maskPhone(wk.Phone),
		"events":        events,
	}
	if self {
		// 本人可见自己的全名
		detail["name"] = wk.FullName
		detail["name_masked"] = false
	} else {
		detail["name"] = maskName(wk.FullName)
		detail["name_masked"] = true
	}
	// 当前用户可执行的流转动作（前端据此渲染按钮）
	allowed := []string{}
	for _, to := range allowedTransitions[WorkerStatus(wk.Status)] {
		if canTransition(u, wk, to) == nil {
			allowed = append(allowed, string(to))
		}
	}
	detail["allowed_transitions"] = allowed
	detail["can_reveal"] = canRevealName(u) && !self

	// 查看留痕：总包/监理/监管员可见
	if u.Role == RoleGCAdmin || u.Role == RoleSupervisor || u.Role == RoleRegulator {
		logs := []map[string]any{}
		lrows, err := s.db.Query(`
			SELECT viewer_name, viewer_role, reason, created_at
			FROM reveal_logs WHERE worker_id = ? ORDER BY id DESC LIMIT 50`, wk.ID)
		if err == nil {
			defer lrows.Close()
			for lrows.Next() {
				var vn, vr, reason, createdAt string
				if lrows.Scan(&vn, &vr, &reason, &createdAt) == nil {
					logs = append(logs, map[string]any{
						"viewer_name": vn, "viewer_role": vr,
						"viewer_role_label": roleLabels[vr],
						"reason":            reason, "created_at": createdAt,
					})
				}
			}
		}
		detail["reveal_logs"] = logs
	}
	writeJSON(w, http.StatusOK, map[string]any{"worker": detail, "server_time": nowUTC()})
}

// ---------- 状态流转（含幂等） ----------

type transitionReq struct {
	To           string `json:"to"`
	Reason       string `json:"reason"`
	ClientOpID   string `json:"client_op_id"`
	ClientTime   string `json:"client_time"`
	FaceVerified bool   `json:"face_verified"`
	IDVerified   bool   `json:"id_verified"`
}

type transitionResult struct {
	WorkerID      int64  `json:"worker_id"`
	To            string `json:"to"`
	ToLabel       string `json:"to_label"`
	CurrentStatus string `json:"current_status"`
	Version       int    `json:"version"`
	EventID       int64  `json:"event_id"`
	Duplicate     bool   `json:"duplicate"`
}

// applyTransition 核心流转：事务内完成 幂等检查→状态机校验→权限→更新→事件→幂等记录
func (s *server) applyTransition(actor *User, workerID int64, req transitionReq) (*transitionResult, *apiError) {
	to := WorkerStatus(req.To)
	if _, ok := statusLabels[to]; !ok {
		return nil, &apiError{HTTPCode: 400, Msg: "未知的目标状态"}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, &apiError{HTTPCode: 500, Msg: "系统繁忙，请重试"}
	}
	defer tx.Rollback()

	// 1) 幂等：同一 client_op_id 直接返回首次结果，不产生重复记录
	if req.ClientOpID != "" {
		var stored string
		err := tx.QueryRow(`SELECT result FROM client_ops WHERE client_op_id = ?`, req.ClientOpID).Scan(&stored)
		if err == nil {
			var res transitionResult
			if json.Unmarshal([]byte(stored), &res) == nil {
				res.Duplicate = true
				tx.Rollback()
				return &res, nil
			}
		}
	}

	// 2) 锁定并读取工人当前状态
	wk := &Worker{}
	var face, idv int
	err = tx.QueryRow(`
		SELECT id, site_id, job_no, full_name, team, status, face_verified, id_verified, version
		FROM workers WHERE id = ?`, workerID).
		Scan(&wk.ID, &wk.SiteID, &wk.JobNo, &wk.FullName, &wk.Team, &wk.Status, &face, &idv, &wk.Version)
	if err == sql.ErrNoRows {
		return nil, &apiError{HTTPCode: 404, Msg: "档案不存在或已被删除"}
	}
	if err != nil {
		return nil, &apiError{HTTPCode: 500, Msg: "系统繁忙，请重试"}
	}

	// 3) 权限
	if aerr := canTransition(actor, wk, to); aerr != nil {
		return nil, aerr
	}

	// 4) 状态机校验（非法回退拦下说原因）
	from := WorkerStatus(wk.Status)
	if aerr := checkTransition(from, to); aerr != nil {
		return nil, aerr
	}

	// 5) 实名制入场：必须完成刷脸 + 身份证核验
	if from == StatusPending && to == StatusOnsite {
		faceOK := face == 1 || req.FaceVerified
		idOK := idv == 1 || req.IDVerified
		if !faceOK || !idOK {
			return nil, &apiError{HTTPCode: 422, Msg: "实名制入场需完成「刷脸核验」与「身份证核验」后方可进场", Current: string(from), Allowed: allowedFrom(from)}
		}
		face, idv = 1, 1
	}

	// 6) 乐观锁更新
	res, err := tx.Exec(`
		UPDATE workers SET status = ?, version = version + 1,
		       face_verified = ?, id_verified = ?, updated_at = ?
		WHERE id = ? AND version = ?`,
		string(to), face, idv, nowUTC(), wk.ID, wk.Version)
	if err != nil {
		return nil, &apiError{HTTPCode: 500, Msg: "系统繁忙，请重试"}
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, &apiError{HTTPCode: 409, Msg: "档案已被他人变更，请刷新后重试", Current: wk.Status, Allowed: allowedFrom(from)}
	}

	// 7) 状态事件
	var opID any
	if req.ClientOpID != "" {
		opID = req.ClientOpID
	}
	er, err := tx.Exec(`
		INSERT INTO status_events(worker_id, from_status, to_status, actor_id, actor_name, actor_role, reason, client_op_id, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`,
		wk.ID, string(from), string(to), actor.ID, actor.Name, actor.Role, strings.TrimSpace(req.Reason), opID, nowUTC())
	if err != nil {
		return nil, &apiError{HTTPCode: 500, Msg: "系统繁忙，请重试"}
	}
	eventID, _ := er.LastInsertId()

	result := &transitionResult{
		WorkerID: wk.ID, To: string(to), ToLabel: statusLabel(string(to)),
		CurrentStatus: string(to), Version: wk.Version + 1, EventID: eventID,
	}

	// 8) 记录幂等键（与业务写入同事务）
	if req.ClientOpID != "" {
		b, _ := json.Marshal(result)
		if _, err := tx.Exec(`
			INSERT INTO client_ops(client_op_id, actor_id, worker_id, to_status, result, created_at)
			VALUES(?,?,?,?,?,?)`,
			req.ClientOpID, actor.ID, wk.ID, string(to), string(b), nowUTC()); err != nil {
			return nil, &apiError{HTTPCode: 500, Msg: "系统繁忙，请重试"}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, &apiError{HTTPCode: 500, Msg: "系统繁忙，请重试"}
	}
	return result, nil
}

func (s *server) handleTransition(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "档案编号格式错误")
		return
	}
	var req transitionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	res, aerr := s.applyTransition(u, id, req)
	if aerr != nil {
		aerr.write(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "server_time": nowUTC()})
}

// ---------- 查看全名（二次确认 + 理由 + 留痕） ----------

type revealReq struct {
	Reason string `json:"reason"`
}

func (s *server) handleRevealName(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !canRevealName(u) {
		writeErr(w, http.StatusForbidden, "仅总包管理员或监理可申请查看全名")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "档案编号格式错误")
		return
	}
	var req revealReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if len([]rune(req.Reason)) < 2 {
		writeErr(w, http.StatusBadRequest, "请填写查看理由（不少于2个字），本次查看将留痕")
		return
	}
	wk, err := s.getWorker(id)
	if err == sql.ErrNoRows {
		writeErr(w, http.StatusNotFound, "档案不存在或已被删除")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询失败")
		return
	}
	if aerr := canViewWorker(u, wk); aerr != nil {
		aerr.write(w)
		return
	}
	if _, err := s.db.Exec(`
		INSERT INTO reveal_logs(worker_id, viewer_id, viewer_name, viewer_role, reason, created_at)
		VALUES(?,?,?,?,?,?)`, wk.ID, u.ID, u.Name, u.Role, req.Reason, nowUTC()); err != nil {
		writeErr(w, http.StatusInternalServerError, "系统繁忙，请重试")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"full_name": wk.FullName,
		"notice":    "本次查看已留痕，仅用于业务需要，请勿外传",
	})
}
