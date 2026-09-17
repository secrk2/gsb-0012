package main

// ---------- 角色 ----------

const (
	RoleGCAdmin    = "gc_admin"   // 总包管理员
	RoleSubLeader  = "sub_leader" // 分包班组长
	RoleSupervisor = "supervisor" // 监理
	RoleWorker     = "worker"     // 工人
	RoleRegulator  = "regulator"  // 监管员
)

var roleLabels = map[string]string{
	RoleGCAdmin:    "总包管理员",
	RoleSubLeader:  "分包班组长",
	RoleSupervisor: "监理",
	RoleWorker:     "工人",
	RoleRegulator:  "监管员",
}

// ---------- 人员状态机 ----------

type WorkerStatus string

const (
	StatusPending     WorkerStatus = "pending"     // 待入场
	StatusOnsite      WorkerStatus = "onsite"      // 在场
	StatusLeave       WorkerStatus = "leave"       // 请假离场
	StatusExited      WorkerStatus = "exited"      // 退场
	StatusBlacklisted WorkerStatus = "blacklisted" // 黑名单
)

var statusLabels = map[WorkerStatus]string{
	StatusPending:     "待入场",
	StatusOnsite:      "在场",
	StatusLeave:       "请假离场",
	StatusExited:      "退场",
	StatusBlacklisted: "黑名单",
}

// allowedTransitions 状态机有向边：只允许沿箭头前进，禁止回退
var allowedTransitions = map[WorkerStatus][]WorkerStatus{
	StatusPending:     {StatusOnsite, StatusBlacklisted},
	StatusOnsite:      {StatusLeave, StatusExited, StatusBlacklisted},
	StatusLeave:       {StatusOnsite, StatusExited, StatusBlacklisted},
	StatusExited:      {},
	StatusBlacklisted: {},
}

func statusLabel(s string) string {
	if l, ok := statusLabels[WorkerStatus(s)]; ok {
		return l
	}
	return s
}

func allowedFrom(s WorkerStatus) []string {
	out := []string{}
	for _, t := range allowedTransitions[s] {
		out = append(out, string(t))
	}
	return out
}

// checkTransition 校验状态流转合法性，非法时返回带原因的错误（拦下并说原因）
func checkTransition(from, to WorkerStatus) *apiError {
	if from == to {
		return &apiError{HTTPCode: 409, Msg: "目标状态与当前状态相同，无需变更", Current: string(from), Allowed: allowedFrom(from)}
	}
	for _, t := range allowedTransitions[from] {
		if t == to {
			return nil
		}
	}
	// 针对常见非法回退给出具体原因
	var msg string
	switch {
	case from == StatusExited:
		msg = "该工人已退场，状态不可回退；如需返岗，请重新建档并走实名制入场流程"
	case from == StatusBlacklisted:
		msg = "该工人已在黑名单中，需监管方解除黑名单后方可变更状态"
	case to == StatusPending:
		msg = "状态不可回退到「待入场」；入场核验一旦完成不可撤销"
	case from == StatusPending && to == StatusLeave:
		msg = "该工人尚未入场，不能办理请假"
	case from == StatusPending && to == StatusExited:
		msg = "该工人尚未入场，无需办理退场；如不再录用可直接拉黑或删除档案"
	default:
		msg = "不允许从「" + statusLabel(string(from)) + "」变更为「" + statusLabel(string(to)) + "」"
	}
	return &apiError{HTTPCode: 409, Msg: msg, Current: string(from), Allowed: allowedFrom(from)}
}

// canTransition 角色权限矩阵：谁可以对哪个人做状态变更
func canTransition(u *User, w *Worker, to WorkerStatus) *apiError {
	switch u.Role {
	case RoleGCAdmin, RoleSupervisor:
		if u.SiteID == nil || *u.SiteID != w.SiteID {
			return &apiError{HTTPCode: 403, Msg: "无权操作其他工地的人员"}
		}
		return nil
	case RoleSubLeader:
		if u.SiteID == nil || *u.SiteID != w.SiteID || u.Team != w.Team {
			return &apiError{HTTPCode: 403, Msg: "班组长只能操作本班组人员"}
		}
		if to == StatusBlacklisted {
			return &apiError{HTTPCode: 403, Msg: "班组长无权将人员拉入黑名单，请联系总包或监理"}
		}
		return nil
	case RoleWorker:
		return &apiError{HTTPCode: 403, Msg: "工人账号无权进行状态变更"}
	case RoleRegulator:
		return &apiError{HTTPCode: 403, Msg: "监管员账号为只读，不能变更人员状态"}
	}
	return &apiError{HTTPCode: 403, Msg: "当前角色无权进行状态变更"}
}

// canRevealName 仅总包管理员/监理可二次确认查看全名
func canRevealName(u *User) bool {
	return u.Role == RoleGCAdmin || u.Role == RoleSupervisor
}

// ---------- 数据模型 ----------

type Site struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	GCCompany string `json:"gc_company"`
}

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Role      string `json:"role"`
	RoleLabel string `json:"role_label"`
	Name      string `json:"name"`
	SiteID    *int64 `json:"site_id"`
	Team      string `json:"team"`
	WorkerID  *int64 `json:"worker_id"`
}

type Worker struct {
	ID           int64  `json:"id"`
	SiteID       int64  `json:"site_id"`
	JobNo        string `json:"job_no"`
	FullName     string `json:"-"` // 全名永不直接序列化，必须走 reveal 流程
	IDCard       string `json:"-"`
	Phone        string `json:"-"`
	Team         string `json:"team"`
	Trade        string `json:"trade"`
	Status       string `json:"status"`
	FaceVerified bool   `json:"face_verified"`
	IDVerified   bool   `json:"id_verified"`
	Version      int    `json:"version"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}
