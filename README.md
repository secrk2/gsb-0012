# 工瞳 · 工地人员实名制与不安全行为预警平台

面向住建场景的工地安全产品骨架。当前包含两大模块：**工地作战台**（在场漏斗 / 今日应培训 / 隐患 / 告警红点）与**人员档案**（实名制状态机 / 姓名脱敏 / 查看留痕），支持离线作业与五类角色数据隔离。

## 一键启动

```bash
docker compose up --build
```

- 前端（Nginx，反代 `/api` → 后端）：http://localhost:8105
- 后端（Go，SQLite 落盘在 `gt-data` 卷）：http://localhost:7105/api/healthz

首次启动自动灌入演示数据：**3 个工地、21 名人员（覆盖待入场/在场/请假离场/退场/黑名单全部状态）、6 个账号、培训/隐患/告警数据**。

## 演示账号（密码统一 `gt123456`）

| 账号 | 角色 | 数据范围 |
|---|---|---|
| `gc_admin` | 总包管理员（王建国） | 城东安置房三期，全部人员，可看全名（留痕） |
| `gc_admin2` | 总包管理员（刘志远） | 滨江商业综合体（用于验证跨工地隔离） |
| `sub_leader` | 分包班组长（李铁军） | 本班组（宏宇劳务·钢筋一班，4 人） |
| `supervisor` | 监理（陈明远） | 城东安置房三期，可看全名（留痕） |
| `worker01` | 工人（张伟） | 仅本人档案 |
| `regulator` | 监管员（赵正） | 全部工地只读，可看查看留痕 |

## 需求点对照

| 需求 | 实现 |
|---|---|
| 人员状态机 | `待入场→在场→请假离场→退场→黑名单`，有向边见 `backend/models.go`；非法回退返回 409 + 中文原因（如「已退场，状态不可回退」） |
| 实名制入场 | `待入场→在场` 必须同时完成刷脸 + 身份证核验，否则 422 拦截 |
| 姓名脱敏 | 列表/详情默认 `张* · 工号`，身份证/手机号同步脱敏 |
| 查看全名 | 仅总包/监理，弹窗二次确认 + 必填理由，写 `reveal_logs` 留痕，监管员可追溯 |
| 作战台 | 在场漏斗、今日应培训、未闭环隐患、告警红点（点击消红点/全部已读） |
| 响应式 | 1440 三栏 / 1024 两栏 / 390 单栏 + 底部导航（断点 1280/768） |
| 数据隔离 | 工人越权看他人档案 → 403 错误态页面（非空白）；班组长限本班组；总包/监理限本工地 |
| 离线作业 | 离线横幅 + 缓存数据打「离线缓存·更新于 xx」戳；变更进 localStorage 队列 |
| 恢复合并 | 恢复后 `POST /api/sync/batch` 批量同步，按 `client_op_id` 幂等去重（唯一索引 + 同事务写入），状态已被他人推进的返回 `conflict` + 当前状态，前端提示并刷新 |

## 离线能力演示

1. 用 `sub_leader` 登录，打开某工人档案；
2. 浏览器 DevTools → Network → 勾选 **Offline**；
3. 顶部出现离线横幅；执行「办理请假」→ 提示「已离线排队」；
4. 取消 Offline → 自动批量同步，提示已同步/去重；
5. 若离线期间他人已变更该工人状态（可开另一浏览器操作），同步时提示冲突并按服务器最新状态合并。

## 本地开发（不用 Docker）

```bash
# 后端（Go ≥ 1.22）
cd backend && go run .            # 监听 :7105，自动建库+种子数据

# 前端（任意静态服务器，需把 /api 代理到 7105，或直接改 js/api.js 中路径）
cd frontend/static && python3 -m http.server 8105
```

## 冒烟测试

后端 29 项断言（登录/隔离/状态机/幂等/留痕/冲突合并）：

```bash
cd backend
GT_DB_PATH=/tmp/gt-smoke.db go run . &   # 用临时库起服务
bash smoke.sh
```

## 目录结构

```
├── docker-compose.yml        # 一键编排：backend(7105) + frontend(8105)
├── backend/                  # Go 后端（标准库 ServeMux + modernc.org/sqlite）
│   ├── main.go               # 路由与中间件
│   ├── models.go             # 角色、状态机、权限矩阵
│   ├── workers.go            # 档案/流转/看全名（核心：applyTransition 事务）
│   ├── dashboard.go          # 作战台聚合
│   ├── sync.go               # 离线批量同步（幂等/冲突）
│   ├── seed.go               # 演示数据
│   └── smoke.sh              # 29 项 API 冒烟
└── frontend/
    ├── nginx.conf            # 静态托管 + /api 反代
    └── static/
        ├── js/api.js         # 鉴权/离线检测/读缓存/变更队列
        ├── js/app.js         # 路由 + 应用壳 + 离线横幅
        └── js/views/         # login / dashboard / workers / workerDetail
```

## API 摘要

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/login` | 登录（bcrypt 校验，12h 会话） |
| GET | `/api/dashboard?site_id=` | 作战台聚合 |
| GET | `/api/workers?status=&q=` | 人员列表（按角色隔离，脱敏） |
| GET | `/api/workers/{id}` / `/api/workers/me` | 档案详情 + 时间线 + 留痕 |
| POST | `/api/workers/{id}/transition` | 状态变更（状态机校验 + `client_op_id` 幂等） |
| POST | `/api/workers/{id}/reveal-name` | 查看全名（总包/监理 + 理由 + 留痕） |
| POST | `/api/sync/batch` | 离线批量同步（applied/duplicate/conflict） |
| POST | `/api/alerts/{id}/read`、`/api/alerts/read-all` | 告警消红点 |
