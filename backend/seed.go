package main

import (
	"database/sql"
	"log"
	"time"
)

func hoursAgo(n int) string {
	return time.Now().UTC().Add(-time.Duration(n) * time.Hour).Format(time.RFC3339)
}
func daysAgo(n int) string { return hoursAgo(n * 24) }

type seedWorker struct {
	jobNo, name, idCard, phone, team, trade, status string
	faceOK, idOK                                    bool
	createdDays                                     int
}

// seed 首次启动时灌入演示数据：3个工地、5类账号、全状态人员、培训/隐患/告警
func seed(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sites`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	log.Println("seed: 写入预置演示数据...")
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// ---------- 工地 ----------
	sites := []struct{ code, name, addr, gc string }{
		{"GT-001", "城东安置房三期项目", "城东区兴业路88号", "华建集团总承包部"},
		{"GT-002", "滨江商业综合体", "滨江区江澜路1号", "华建集团第二工程部"},
		{"GT-003", "地铁12号线车辆段工程", "经开区车辆段南路", "中铁城建联合体"},
	}
	siteIDs := map[string]int64{}
	for _, s := range sites {
		res, err := tx.Exec(`INSERT INTO sites(code, name, address, gc_company) VALUES(?,?,?,?)`, s.code, s.name, s.addr, s.gc)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		siteIDs[s.code] = id
	}
	s1, s2, s3 := siteIDs["GT-001"], siteIDs["GT-002"], siteIDs["GT-003"]

	// ---------- 人员 ----------
	teamA := "宏宇劳务·钢筋一班"
	teamB := "宏宇劳务·木工二班"
	teamC := "中建劳务·架子班"
	teamD := "安捷租赁·机械班"

	workers := []struct {
		siteID int64
		w      seedWorker
	}{
		// 工地一：10人，覆盖全部状态
		{s1, seedWorker{"GT-1001", "张伟", "320102198503124321", "13812340001", teamA, "钢筋工", "onsite", true, true, 12}},
		{s1, seedWorker{"GT-1002", "李强", "320102199011056782", "13812340002", teamA, "钢筋工", "onsite", true, true, 11}},
		{s1, seedWorker{"GT-1003", "王军", "410203198709233415", "13812340003", teamB, "木工", "onsite", true, true, 8}},
		{s1, seedWorker{"GT-1004", "赵勇", "410203199205128876", "13812340004", teamC, "架子工", "leave", true, true, 15}},
		{s1, seedWorker{"GT-1005", "陈刚", "510104198812304455", "13812340005", teamA, "电工", "pending", false, false, 2}},
		{s1, seedWorker{"GT-1006", "刘明", "510104197911172233", "13812340006", teamD, "塔吊司机", "onsite", true, true, 20}},
		{s1, seedWorker{"GT-1007", "孙浩", "340106200001097788", "13812340007", teamB, "普工", "pending", false, false, 1}},
		{s1, seedWorker{"GT-1008", "周斌", "340106198606251144", "13812340008", teamC, "焊工", "exited", true, true, 30}},
		{s1, seedWorker{"GT-1009", "吴磊", "220181199309086655", "13812340009", teamD, "信号工", "leave", true, true, 12}},
		{s1, seedWorker{"GT-1010", "郑凯", "220181198107143322", "13812340010", teamA, "普工", "blacklisted", true, true, 25}},
		// 工地二：6人
		{s2, seedWorker{"GT-2001", "杨帆", "330106199104126677", "13812340011", teamA, "钢筋工", "onsite", true, true, 14}},
		{s2, seedWorker{"GT-2002", "黄志", "330106198712208899", "13812340012", teamB, "木工", "onsite", true, true, 13}},
		{s2, seedWorker{"GT-2003", "徐亮", "440305199507031122", "13812340013", teamB, "普工", "pending", false, false, 1}},
		{s2, seedWorker{"GT-2004", "何军", "440305198910164433", "13812340014", teamC, "架子工", "onsite", true, true, 10}},
		{s2, seedWorker{"GT-2005", "郭涛", "610113198402285566", "13812340015", teamD, "塔吊司机", "exited", true, true, 40}},
		{s2, seedWorker{"GT-2006", "马超", "610113199601307799", "13812340016", teamA, "钢筋工", "onsite", true, true, 6}},
		// 工地三：5人
		{s3, seedWorker{"GT-3001", "林峰", "350102198805114411", "13812340017", teamA, "钢筋工", "onsite", true, true, 9}},
		{s3, seedWorker{"GT-3002", "谢文", "350102199302276622", "13812340018", teamC, "架子工", "leave", true, true, 16}},
		{s3, seedWorker{"GT-3003", "罗成", "370202199911083344", "13812340019", teamB, "普工", "pending", false, false, 2}},
		{s3, seedWorker{"GT-3004", "高翔", "370202198704195588", "13812340020", teamD, "信号工", "onsite", true, true, 7}},
		{s3, seedWorker{"GT-3005", "唐伟", "500103198510236677", "13812340021", teamA, "电工", "onsite", true, true, 5}},
	}
	workerIDs := map[string]int64{}
	for _, it := range workers {
		w := it.w
		face, idv := 0, 0
		if w.faceOK {
			face = 1
		}
		if w.idOK {
			idv = 1
		}
		res, err := tx.Exec(`
			INSERT INTO workers(site_id, job_no, full_name, id_card, phone, team, trade, status,
			                    face_verified, id_verified, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			it.siteID, w.jobNo, w.name, w.idCard, w.phone, w.team, w.trade, w.status,
			face, idv, daysAgo(w.createdDays), daysAgo(1))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		workerIDs[w.jobNo] = id
	}

	// ---------- 账号（五类角色，演示密码统一 gt123456） ----------
	pw := hashPassword("gt123456")
	type seedUser struct {
		username, role, name, team string
		siteID                     *int64
		workerID                   *int64
	}
	i64 := func(v int64) *int64 { return &v }
	zhangWeiID := workerIDs["GT-1001"]
	users := []seedUser{
		{"gc_admin", RoleGCAdmin, "王建国", "", i64(s1), nil},
		{"gc_admin2", RoleGCAdmin, "刘志远", "", i64(s2), nil},
		{"sub_leader", RoleSubLeader, "李铁军", teamA, i64(s1), nil},
		{"supervisor", RoleSupervisor, "陈明远", "", i64(s1), nil},
		{"worker01", RoleWorker, "张伟", teamA, i64(s1), &zhangWeiID},
		{"regulator", RoleRegulator, "赵正", "", nil, nil},
	}
	userIDs := map[string]int64{}
	for _, u := range users {
		res, err := tx.Exec(`
			INSERT INTO users(username, password_hash, role, name, site_id, team, worker_id)
			VALUES(?,?,?,?,?,?,?)`,
			u.username, pw, u.role, u.name, u.siteID, u.team, u.workerID)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		userIDs[u.username] = id
	}

	// ---------- 状态流转历史 ----------
	type ev struct {
		jobNo, from, to, actor, role, reason string
		days                                 int
	}
	events := []ev{
		{"GT-1001", "none", "pending", "李铁军", RoleSubLeader, "建档登记，待实名制入场", 12},
		{"GT-1001", "pending", "onsite", "李铁军", RoleSubLeader, "实名制入场：刷脸+身份证核验通过", 10},
		{"GT-1002", "none", "pending", "李铁军", RoleSubLeader, "建档登记", 11},
		{"GT-1002", "pending", "onsite", "李铁军", RoleSubLeader, "实名制入场：刷脸+身份证核验通过", 9},
		{"GT-1003", "none", "pending", "王建国", RoleGCAdmin, "建档登记", 8},
		{"GT-1003", "pending", "onsite", "王建国", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 8},
		{"GT-1004", "none", "pending", "王建国", RoleGCAdmin, "建档登记", 15},
		{"GT-1004", "pending", "onsite", "王建国", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 14},
		{"GT-1004", "onsite", "leave", "王建国", RoleGCAdmin, "家中有事，请假3天", 1},
		{"GT-1005", "none", "pending", "李铁军", RoleSubLeader, "建档登记，待实名制核验", 2},
		{"GT-1006", "none", "pending", "王建国", RoleGCAdmin, "建档登记", 20},
		{"GT-1006", "pending", "onsite", "王建国", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 19},
		{"GT-1007", "none", "pending", "王建国", RoleGCAdmin, "建档登记，待实名制核验", 1},
		{"GT-1008", "none", "pending", "王建国", RoleGCAdmin, "建档登记", 30},
		{"GT-1008", "pending", "onsite", "王建国", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 29},
		{"GT-1008", "onsite", "exited", "王建国", RoleGCAdmin, "主体结构节点完成，办理退场", 5},
		{"GT-1009", "none", "pending", "陈明远", RoleSupervisor, "建档登记", 12},
		{"GT-1009", "pending", "onsite", "陈明远", RoleSupervisor, "实名制入场：刷脸+身份证核验通过", 11},
		{"GT-1009", "onsite", "leave", "陈明远", RoleSupervisor, "家中急事请假", 2},
		{"GT-1010", "none", "pending", "李铁军", RoleSubLeader, "建档登记", 25},
		{"GT-1010", "pending", "onsite", "李铁军", RoleSubLeader, "实名制入场：刷脸+身份证核验通过", 24},
		{"GT-1010", "onsite", "blacklisted", "王建国", RoleGCAdmin, "多次酒后上岗，触碰安全红线，列入黑名单", 3},
		{"GT-2001", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 14},
		{"GT-2001", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 13},
		{"GT-2002", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 13},
		{"GT-2002", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 12},
		{"GT-2003", "none", "pending", "刘志远", RoleGCAdmin, "建档登记，待实名制核验", 1},
		{"GT-2004", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 10},
		{"GT-2004", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 9},
		{"GT-2005", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 40},
		{"GT-2005", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 38},
		{"GT-2005", "onsite", "exited", "刘志远", RoleGCAdmin, "合同到期退场", 7},
		{"GT-2006", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 6},
		{"GT-2006", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 5},
		{"GT-3001", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 9},
		{"GT-3001", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 8},
		{"GT-3002", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 16},
		{"GT-3002", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 15},
		{"GT-3002", "onsite", "leave", "刘志远", RoleGCAdmin, "病假一周", 3},
		{"GT-3003", "none", "pending", "刘志远", RoleGCAdmin, "建档登记，待实名制核验", 2},
		{"GT-3004", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 7},
		{"GT-3004", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 6},
		{"GT-3005", "none", "pending", "刘志远", RoleGCAdmin, "建档登记", 5},
		{"GT-3005", "pending", "onsite", "刘志远", RoleGCAdmin, "实名制入场：刷脸+身份证核验通过", 4},
	}
	for _, e := range events {
		if _, err := tx.Exec(`
			INSERT INTO status_events(worker_id, from_status, to_status, actor_name, actor_role, reason, created_at)
			VALUES(?,?,?,?,?,?,?)`,
			workerIDs[e.jobNo], e.from, e.to, e.actor, e.role, e.reason, daysAgo(e.days)); err != nil {
			return err
		}
	}

	// ---------- 查看全名留痕示例 ----------
	if _, err := tx.Exec(`
		INSERT INTO reveal_logs(worker_id, viewer_id, viewer_name, viewer_role, reason, created_at)
		VALUES(?,?,?,?,?,?)`,
		workerIDs["GT-1010"], userIDs["gc_admin"], "王建国", RoleGCAdmin,
		"黑名单人员约谈，需核对身份证信息", daysAgo(2)); err != nil {
		return err
	}

	// ---------- 培训（今日应培训） ----------
	today := todayLocal()
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	trainings := []struct {
		siteID     int64
		jobNo      string
		title, due string
		status     string
	}{
		{s1, "GT-1001", "高处作业安全专项培训", today, "pending"},
		{s1, "GT-1002", "高处作业安全专项培训", today, "pending"},
		{s1, "GT-1003", "高处作业安全专项培训", today, "pending"},
		{s1, "GT-1005", "入场三级安全教育", today, "pending"},
		{s1, "GT-1007", "入场三级安全教育", today, "pending"},
		{s1, "GT-1006", "塔吊作业安全交底", yesterday, "done"},
		{s2, "GT-2001", "起重吊装作业安全培训", today, "pending"},
		{s2, "GT-2004", "脚手架搭设安全培训", today, "pending"},
		{s3, "GT-3001", "有限空间作业安全培训", today, "pending"},
	}
	for _, t := range trainings {
		if _, err := tx.Exec(`
			INSERT INTO trainings(site_id, worker_id, title, due_date, status)
			VALUES(?,?,?,?,?)`,
			t.siteID, workerIDs[t.jobNo], t.title, t.due, t.status); err != nil {
			return err
		}
	}

	// ---------- 隐患 ----------
	hazards := []struct {
		siteID               int64
		title, level, status string
		days                 int
	}{
		{s1, "3#楼东侧临边防护栏杆缺失", "较大", "open", 2},
		{s1, "2#配电箱私拉乱接", "一般", "rectifying", 4},
		{s1, "基坑西侧边坡局部渗水，存在塌方风险", "重大", "open", 1},
		{s2, "脚手架连墙件数量不足", "较大", "open", 3},
		{s2, "材料堆放占用消防通道", "一般", "open", 1},
		{s3, "盾构区间气体检测记录缺失", "一般", "open", 2},
	}
	for _, h := range hazards {
		if _, err := tx.Exec(`
			INSERT INTO hazards(site_id, title, level, status, created_at)
			VALUES(?,?,?,?,?)`,
			h.siteID, h.title, h.level, h.status, daysAgo(h.days)); err != nil {
			return err
		}
	}

	// ---------- 告警（红点） ----------
	alerts := []struct {
		siteID   int64
		typ, msg string
		level    string
		read     int
		hours    int
	}{
		{s1, "ai_behavior", "AI识别：3#楼作业面2名工人未佩戴安全帽", "critical", 0, 2},
		{s1, "device", "1#塔吊吊钩可视化设备离线", "warning", 0, 5},
		{s1, "access", "闸机拦截1名非实名人员入场", "critical", 1, 26},
		{s2, "ai_behavior", "AI识别：吊装作业半径内人员闯入", "warning", 0, 3},
		{s3, "device", "扬尘在线监测PM10超标", "warning", 0, 6},
	}
	for _, a := range alerts {
		if _, err := tx.Exec(`
			INSERT INTO alerts(site_id, type, message, level, read, created_at)
			VALUES(?,?,?,?,?,?)`,
			a.siteID, a.typ, a.msg, a.level, a.read, hoursAgo(a.hours)); err != nil {
			return err
		}
	}

	log.Printf("seed: 完成（%d工地 / %d人员 / %d账号），开始生成出勤打卡与分账演示数据...", len(sites), len(workers), len(users))
	if err := seedAttendanceBase(tx, siteIDs, workerIDs, userIDs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := seedAttendancePayroll(db); err != nil {
		return err
	}
	log.Printf("seed: 出勤打卡与分账演示数据完成")
	return nil
}
