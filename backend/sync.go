package main

import (
	"encoding/json"
	"net/http"
)

// 离线恢复批量同步：每个操作带 client_op_id，服务端幂等去重；
// 状态已被他人推进的返回 conflict 并携带当前状态，由前端刷新合并。

type syncOp struct {
	ClientOpID   string `json:"client_op_id"`
	WorkerID     int64  `json:"worker_id"`
	To           string `json:"to"`
	Reason       string `json:"reason"`
	ClientTime   string `json:"client_time"`
	FaceVerified bool   `json:"face_verified"`
	IDVerified   bool   `json:"id_verified"`
}

type syncReq struct {
	Ops []syncOp `json:"ops"`
}

type syncResult struct {
	ClientOpID    string   `json:"client_op_id"`
	WorkerID      int64    `json:"worker_id"`
	Status        string   `json:"status"` // applied / duplicate / conflict / error
	Message       string   `json:"message,omitempty"`
	CurrentStatus string   `json:"current_status,omitempty"`
	Allowed       []string `json:"allowed,omitempty"`
}

func (s *server) handleSyncBatch(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == RoleWorker || u.Role == RoleRegulator {
		writeErr(w, http.StatusForbidden, "当前角色无权同步状态变更")
		return
	}
	var req syncReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if len(req.Ops) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"results": []syncResult{}})
		return
	}
	if len(req.Ops) > 200 {
		writeErr(w, http.StatusBadRequest, "单批最多同步200条变更")
		return
	}
	results := make([]syncResult, 0, len(req.Ops))
	for _, op := range req.Ops {
		if op.ClientOpID == "" {
			results = append(results, syncResult{
				ClientOpID: op.ClientOpID, WorkerID: op.WorkerID,
				Status: "error", Message: "缺少 client_op_id，无法保证幂等",
			})
			continue
		}
		res, aerr := s.applyTransition(u, op.WorkerID, transitionReq{
			To:           op.To,
			Reason:       op.Reason,
			ClientOpID:   op.ClientOpID,
			ClientTime:   op.ClientTime,
			FaceVerified: op.FaceVerified,
			IDVerified:   op.IDVerified,
		})
		switch {
		case aerr == nil && res.Duplicate:
			results = append(results, syncResult{
				ClientOpID: op.ClientOpID, WorkerID: op.WorkerID,
				Status: "duplicate", Message: "该变更此前已同步，已去重",
				CurrentStatus: res.CurrentStatus,
			})
		case aerr == nil:
			results = append(results, syncResult{
				ClientOpID: op.ClientOpID, WorkerID: op.WorkerID,
				Status: "applied", Message: "已同步",
				CurrentStatus: res.CurrentStatus,
			})
		case aerr.HTTPCode == 409:
			// 离线期间状态已被他人推进：服务端为准，前端据此刷新
			results = append(results, syncResult{
				ClientOpID: op.ClientOpID, WorkerID: op.WorkerID,
				Status: "conflict", Message: aerr.Msg,
				CurrentStatus: aerr.Current, Allowed: aerr.Allowed,
			})
		default:
			results = append(results, syncResult{
				ClientOpID: op.ClientOpID, WorkerID: op.WorkerID,
				Status: "error", Message: aerr.Msg,
				CurrentStatus: aerr.Current, Allowed: aerr.Allowed,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "server_time": nowUTC()})
}
