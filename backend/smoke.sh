#!/bin/bash
# 工瞳 API 冒烟测试
set -u
BASE=http://localhost:7105
PASS=0; FAIL=0

check() { # name, condition
  if [ "$2" = "0" ]; then echo "  ✅ $1"; PASS=$((PASS+1)); else echo "  ❌ $1"; FAIL=$((FAIL+1)); fi
}

login() { # username -> token
  curl -s -X POST $BASE/api/login -H 'Content-Type: application/json' \
    -d "{\"username\":\"$1\",\"password\":\"gt123456\"}" | python3 -c "import sys,json;print(json.load(sys.stdin).get('token',''))"
}

echo "== 1. 登录五类角色 =="
for u in gc_admin sub_leader supervisor worker01 regulator gc_admin2; do
  T=$(login $u)
  [ -n "$T" ]; check "登录 $u" $?
done

GC=$(login gc_admin); SL=$(login sub_leader); SP=$(login supervisor); WK=$(login worker01); RG=$(login regulator); GC2=$(login gc_admin2)

echo "== 2. 作战台聚合 =="
OUT=$(curl -s "$BASE/api/dashboard" -H "Authorization: Bearer $GC")
echo "$OUT" | python3 -c "
import sys,json; d=json.load(sys.stdin)
f=d['funnel']
assert d['site']['code']=='GT-001', d['site']
assert f['onsite']==4 and f['pending']==2 and f['leave']==2 and f['exited']==1 and f['blacklisted']==1, f
assert len(d['today_trainings'])==5, len(d['today_trainings'])
assert d['unread_alerts']==2 and d['open_hazards']==3
print('ok')" >/dev/null 2>&1
check "gc_admin 作战台(漏斗4/2/2/1/1, 培训5, 红点2, 隐患3)" $?

OUT=$(curl -s "$BASE/api/dashboard" -H "Authorization: Bearer $RG")
echo "$OUT" | grep -q '"code":"GT-001"'
check "监管员作战台默认首个工地" $?

OUT=$(curl -s "$BASE/api/dashboard?site_id=2" -H "Authorization: Bearer $GC")
echo "$OUT" | grep -q '无权查看其他工地'
check "gc_admin 越权看工地2被拒" $?

echo "== 3. 数据隔离 =="
OUT=$(curl -s "$BASE/api/workers" -H "Authorization: Bearer $SL")
CNT=$(echo "$OUT" | python3 -c "import sys,json;ws=json.load(sys.stdin)['workers'];print(len(ws))")
[ "$CNT" = "4" ]
check "班组长只见本班组(4人)" $?

OUT=$(curl -s "$BASE/api/workers" -H "Authorization: Bearer $WK")
echo "$OUT" | grep -q '无权浏览人员列表'
check "工人查列表被拒" $?

OUT=$(curl -s "$BASE/api/workers/2" -H "Authorization: Bearer $WK")
echo "$OUT" | grep -q '无权查看他人档案'
check "工人看他人档案 403 错误态" $?

OUT=$(curl -s "$BASE/api/workers/me" -H "Authorization: Bearer $WK")
echo "$OUT" | python3 -c "import sys,json;w=json.load(sys.stdin)['worker'];assert w['name']=='张伟' and w['name_masked']==False" 2>/dev/null
check "工人看本人档案见全名" $?

OUT=$(curl -s "$BASE/api/workers/1" -H "Authorization: Bearer $GC")
echo "$OUT" | python3 -c "
import sys,json; w=json.load(sys.stdin)['worker']
assert w['name']=='张*' and w['name_masked']==True, w['name']
assert w['can_reveal']==True
assert 'full_name' not in json.dumps(w)
assert w['id_card'].startswith('3201') and '*' in w['id_card']
print('ok')" >/dev/null 2>&1
check "总包看档案脱敏(张*)" $?

echo "== 4. 状态机 =="
# 退场→在场 非法回退
OUT=$(curl -s -X POST "$BASE/api/workers/8/transition" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' -d '{"to":"onsite","reason":"test"}')
echo "$OUT" | grep -q '已退场，状态不可回退'
check "退场→在场 拦截并说原因" $?

# 待入场→请假 非法
OUT=$(curl -s -X POST "$BASE/api/workers/5/transition" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' -d '{"to":"leave"}')
echo "$OUT" | grep -q '尚未入场，不能办理请假'
check "待入场→请假 拦截并说原因" $?

# 黑名单→在场 非法
OUT=$(curl -s -X POST "$BASE/api/workers/10/transition" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' -d '{"to":"onsite"}')
echo "$OUT" | grep -q '黑名单'
check "黑名单→在场 拦截并说原因" $?

# 待入场→在场 未核验
OUT=$(curl -s -X POST "$BASE/api/workers/5/transition" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' -d '{"to":"onsite"}')
echo "$OUT" | grep -q '刷脸核验'
check "未刷脸/身份证 入场被拒(422)" $?

# 班组长拉黑 越权
OUT=$(curl -s -X POST "$BASE/api/workers/1/transition" -H "Authorization: Bearer $SL" -H 'Content-Type: application/json' -d '{"to":"blacklisted","reason":"x"}')
echo "$OUT" | grep -q '班组长无权'
check "班组长拉黑被拒" $?

# 班组长操作别班组人员
OUT=$(curl -s -X POST "$BASE/api/workers/3/transition" -H "Authorization: Bearer $SL" -H 'Content-Type: application/json' -d '{"to":"leave","reason":"x"}')
echo "$OUT" | grep -q '本班组'
check "班组长操作别班组被拒" $?

# 合法：待入场→在场（核验通过）+ 幂等
OPID="smoke-$(date +%s)-entry"
OUT=$(curl -s -X POST "$BASE/api/workers/5/transition" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d "{\"to\":\"onsite\",\"reason\":\"实名制入场\",\"face_verified\":true,\"id_verified\":true,\"client_op_id\":\"$OPID\"}")
echo "$OUT" | python3 -c "import sys,json;r=json.load(sys.stdin)['result'];assert r['current_status']=='onsite' and r['duplicate']==False" 2>/dev/null
check "合法入场(刷脸+身份证)" $?

OUT2=$(curl -s -X POST "$BASE/api/workers/5/transition" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d "{\"to\":\"onsite\",\"reason\":\"实名制入场\",\"face_verified\":true,\"id_verified\":true,\"client_op_id\":\"$OPID\"}")
echo "$OUT2" | python3 -c "import sys,json;r=json.load(sys.stdin)['result'];assert r['duplicate']==True" 2>/dev/null
check "同 client_op_id 重放→去重" $?

CNT=$(curl -s "$BASE/api/workers/5" -H "Authorization: Bearer $GC" | python3 -c "import sys,json;print(len(json.load(sys.stdin)['worker']['events']))")
[ "$CNT" = "2" ]
check "重放未产生重复事件(仍2条)" $?

echo "== 5. 查看全名留痕 =="
OUT=$(curl -s -X POST "$BASE/api/workers/1/reveal-name" -H "Authorization: Bearer $WK" -H 'Content-Type: application/json' -d '{"reason":"想看"}')
echo "$OUT" | grep -q '仅总包管理员或监理'
check "工人申请看全名被拒" $?

OUT=$(curl -s -X POST "$BASE/api/workers/1/reveal-name" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' -d '{"reason":""}')
echo "$OUT" | grep -q '请填写查看理由'
check "空理由被拒" $?

OUT=$(curl -s -X POST "$BASE/api/workers/1/reveal-name" -H "Authorization: Bearer $SP" -H 'Content-Type: application/json' -d '{"reason":"工伤认定核对"}')
echo "$OUT" | python3 -c "import sys,json;d=json.load(sys.stdin);assert d['full_name']=='张伟'" 2>/dev/null
check "监理填理由看到全名" $?

OUT=$(curl -s "$BASE/api/workers/1" -H "Authorization: Bearer $RG")
echo "$OUT" | python3 -c "
import sys,json; w=json.load(sys.stdin)['worker']
logs=w.get('reveal_logs',[])
assert any(l['reason']=='工伤认定核对' and l['viewer_name']=='陈明远' for l in logs), logs
print('ok')" >/dev/null 2>&1
check "查看留痕可追溯(监管员可见)" $?

echo "== 6. 离线批量同步（幂等+冲突合并） =="
OP1="sync-$(date +%s)-a"
# 先把 GT-1002 请假（在线）
curl -s -X POST "$BASE/api/workers/2/transition" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"to":"leave","reason":"家中急事","client_op_id":"'$OP1'"}' > /dev/null
# 模拟离线队列重放：同一操作再同步一次 → duplicate
OUT=$(curl -s -X POST "$BASE/api/sync/batch" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"ops":[{"client_op_id":"'$OP1'","worker_id":2,"to":"leave","reason":"家中急事"}]}')
echo "$OUT" | python3 -c "import sys,json;r=json.load(sys.stdin)['results'][0];assert r['status']=='duplicate',r" 2>/dev/null
check "批量同步重放→duplicate 不重复记录" $?

# 冲突：离线期间状态已推进（leave→exited），再同步 leave→onsite 旧操作
curl -s -X POST "$BASE/api/workers/2/transition" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"to":"exited","reason":"提前退场","client_op_id":"sync-'$(date +%s)'-b"}' > /dev/null
OUT=$(curl -s -X POST "$BASE/api/sync/batch" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"ops":[{"client_op_id":"sync-'$(date +%s)'-c","worker_id":2,"to":"onsite","reason":"销假"}]}')
echo "$OUT" | python3 -c "
import sys,json;r=json.load(sys.stdin)['results'][0]
assert r['status']=='conflict' and r['current_status']=='exited', r
print('ok')" >/dev/null 2>&1
check "离线旧操作→conflict 并返回当前状态" $?

echo ""
echo "== 7. 出勤打卡：幂等/合并/判定/隔离 =="
# 幂等
OP="att-$(date +%s)-1"
for i in 1 2; do
  OUT=$(curl -s -X POST "$BASE/api/attendance/punches" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
    -d '{"worker_id":3,"direction":"in","source":"gate","device_id":"SMK-A","punch_time":"2026-09-16T23:50:00Z","client_op_id":"'$OP'"}')
done
echo "$OUT" | grep -q '"duplicate":true'
check "同 client_op_id 打卡重放→幂等去重" $?

# 同设备3分钟去重
curl -s -X POST "$BASE/api/attendance/punches" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"worker_id":3,"direction":"out","source":"gate","device_id":"SMK-A","punch_time":"2026-09-17T01:00:00Z"}' >/dev/null
OUT=$(curl -s -X POST "$BASE/api/attendance/punches" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"worker_id":3,"direction":"out","source":"gate","device_id":"SMK-A","punch_time":"2026-09-17T01:02:00Z"}')
echo "$OUT" | grep -q 'same_device_3min'
check "同设备3分钟内重复刷卡→合并一笔" $?

# 双闸机+手机同日多源 → 甘特只合并为一条
curl -s -X POST "$BASE/api/attendance/punches" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"worker_id":4,"direction":"in","source":"gate","device_id":"GATE-A","punch_time":"2026-09-16T23:55:00Z"}' >/dev/null
curl -s -X POST "$BASE/api/attendance/punches" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"worker_id":4,"direction":"in","source":"gate","device_id":"GATE-B","punch_time":"2026-09-16T23:56:30Z"}' >/dev/null
curl -s -X POST "$BASE/api/attendance/punches" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"worker_id":4,"direction":"out","source":"mobile","device_id":"MOBILE","punch_time":"2026-09-17T09:30:00Z"}' >/dev/null
OUT=$(curl -s "$BASE/api/attendance/gantt?view=week&date=2026-09-17" -H "Authorization: Bearer $GC")
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
p=[x for x in d['people'] if x['worker_id']==4][0]
c=p['cells']['2026-09-17']
assert c['merged'] is True and c['punch_count']==3 and len(c['sources'])==2, c
print('ok')" >/dev/null 2>&1
check "双闸机+手机同日→合并1条(流水3笔/结论1条)" $?

# 迟到判定（GT-1003 王军=id3 为迟到型，9月历史上必有迟到日）
# 缺勤判定：GT-1005=id5 在第4段已实名制入场但从未打卡 → 本周应计缺勤
OUT=$(curl -s "$BASE/api/attendance/gantt?view=month&month=2026-09" -H "Authorization: Bearer $GC")
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
by={x['worker_id']:x for x in d['people']}
c3=[c for c in by[3]['cells'].values() if c['status'] in ('late','late_early')]
assert len(c3) >= 1, '王军9月应至少有1个迟到日'
assert by[3]['late_days'] >= 1
assert by[5]['has_any_punch'] is False and by[5]['absent_days'] >= 1, ('陈刚入场无卡应计缺勤', by[5]['absent_days'])
print('ok')" >/dev/null 2>&1
check "缺勤(入场无卡)与迟到(08:01型)判定正确" $?

# 三种空态：无打卡人(GT-1007=id7 待入场)、当周全废(GT-2006=id16 属工地二，监管员可见)
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
by={x['worker_id']:x for x in d['people']}
assert by[7]['has_any_punch'] is False, by[7]
print('ok')" >/dev/null 2>&1
check "空态：该人无任何打卡（GT-1007）" $?
OUT=$(curl -s "$BASE/api/attendance/gantt?view=week&date=2026-09-14&site_id=2" -H "Authorization: Bearer $RG")
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
by={x['worker_id']:x for x in d['people']}
assert by[16]['all_void'] is True, by[16]
print('ok')" >/dev/null 2>&1
check "空态：该时段打卡全作废（GT-2006 09-14/15）" $?

# 数据隔离
OUT=$(curl -s "$BASE/api/attendance/gantt?view=month&month=2026-09" -H "Authorization: Bearer $SL")
echo "$OUT" | grep -q '宏宇劳务·钢筋一班'
check "班组长可见本班组出勤" $?
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
assert all(x['team']=='宏宇劳务·钢筋一班' for x in d['people']), d['teams']
print('ok')" >/dev/null 2>&1
check "班组长看不到其他班组" $?

# 工人代他人打卡 → 403
OUT=$(curl -s -X POST "$BASE/api/attendance/punches" -H "Authorization: Bearer $WK" -H 'Content-Type: application/json' \
  -d '{"worker_id":2,"direction":"in","source":"mobile"}')
echo "$OUT" | grep -q '只能为本人打卡'
check "工人代他人打卡→403" $?

# 工人甘特只见自己
OUT=$(curl -s "$BASE/api/attendance/gantt?view=week" -H "Authorization: Bearer $WK")
echo "$OUT" | python3 -c "import sys,json;d=json.load(sys.stdin);assert len(d['people'])==1 and d['people'][0]['worker_id']==1" 2>/dev/null
check "工人只看得到本人出勤" $?

# 口径双下发且公式不同
echo "$OUT" | grep -q '实际出勤天数'
check "甘特下发天口径公式" $?
echo "$OUT" | grep -q '有效打卡工时'
check "甘特下发工时口径公式" $?

echo ""
echo "== 8. 分账：版本单价/冻结/部分发/复核 =="
# 6月 v1 300，8月 v2 330，9月 v3 360（张伟 id=1）
OUT=$(curl -s "$BASE/api/payroll?month=2026-06" -H "Authorization: Bearer $GC")
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
assert d['status']=='settled'
zw=[i for i in d['items'] if i['worker_id']==1][0]
assert zw['rate_version']==1 and zw['day_rate']==300, zw
assert abs(zw['amount']-zw['work_days']*300)<0.01
assert zw['paid_amount']>=zw['final_amount']  # 6月发清
print('ok')" >/dev/null 2>&1
check "6月冻结在 v1 ¥300 且已发清" $?

OUT=$(curl -s -X POST "$BASE/api/payroll/settle" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' -d '{"month":"2026-06"}')
echo "$OUT" | grep -q '已结算冻结'
check "重复结算已冻结月→拒绝" $?

OUT=$(curl -s "$BASE/api/payroll?month=2026-08" -H "Authorization: Bearer $GC")
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
zw=[i for i in d['items'] if i['worker_id']==1][0]
assert zw['rate_version']==2 and zw['day_rate']==330, zw
assert zw['amount']==8580, zw['amount']
assert zw['paid_amount']==3000 and zw['pending_amount']==5580, zw
assert zw['dirty'] is True and zw['current_amount']==8250 and zw['current_diff']==-330, zw
print('ok')" >/dev/null 2>&1
check "8月冻结v2/部分发3000不被重算冲掉/差异-330待复核" $?

# 9月实时用 v3 360
OUT=$(curl -s "$BASE/api/payroll?month=2026-09" -H "Authorization: Bearer $GC")
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
zw=[i for i in d['items'] if i['worker_id']==1][0]
assert zw['rate_version']==3 and zw['day_rate']==360, zw
print('ok')" >/dev/null 2>&1
check "新单价v3只影响9月起(张伟¥360)" $?

# 已结算月改动打卡：未确认 409 need_confirm；确认后冻结金额与已发不变
FIRSTPID=$(curl -s "$BASE/api/workers/1/punches?date=2026-08-20" -H "Authorization: Bearer $GC" | python3 -c "import sys,json;print(json.load(sys.stdin)['punches'][0]['id'])")
OUT=$(curl -s -X POST "$BASE/api/punches/$FIRSTPID/void" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' -d '{"reason":"冒烟测试核对"}')
echo "$OUT" | python3 -c "import sys,json;d=json.load(sys.stdin);assert d['need_confirm'] is True" 2>/dev/null
check "改已结算月打卡→先要求二次确认" $?
# 把 08-20 当天 4 笔（闸机+手机）全部作废 → 当天少1工日，实时金额 7920
for PID in $(curl -s "$BASE/api/workers/1/punches?date=2026-08-20" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
for p in json.load(sys.stdin)['punches']:
    if p['status']=='valid': print(p['id'])"); do
  curl -s -X POST "$BASE/api/punches/$PID/void" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
    -d '{"reason":"冒烟测试核对","confirm_settled":true}' >/dev/null
done
# 冻结金额仍是8580、已发仍是3000（不被冲掉）
curl -s "$BASE/api/payroll?month=2026-08" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
d=json.load(sys.stdin)
zw=[i for i in d['items'] if i['worker_id']==1][0]
assert zw['amount']==8580 and zw['paid_amount']==3000 and zw['current_amount']==7920, zw
print('ok')" >/dev/null 2>&1
check "二次确认后：冻结8580/已发3000不变，实时降至7920" $?

# 复核补差（apply）：应发变实时金额，已发3000不动
OUT=$(curl -s -X POST "$BASE/api/payroll/review" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"2026-08","worker_id":1,"action":"apply","reason":"冒烟确认08-20与08-25代打卡，按实际工日补差"}')
echo "$OUT" | python3 -c "
import sys,json
d=json.load(sys.stdin)
assert d['paid_amount']==3000, d
assert d['final_amount']==7920, d
assert d['pending_amount']==4920, d
print('ok')" >/dev/null 2>&1
check "复核补差后应发=7920 已发3000不冲销 待发4920" $?
# 差异标记清除
curl -s "$BASE/api/payroll?month=2026-08" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
d=json.load(sys.stdin)
zw=[i for i in d['items'] if i['worker_id']==1][0]
assert zw['dirty'] is False and zw['final_amount']==7920 and zw['pending_amount']==4920, zw
print('ok')" >/dev/null 2>&1
check "复核后差异标记清除且待发正确" $?

# 无原因复核被拒
OUT=$(curl -s -X POST "$BASE/api/payroll/review" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"2026-08","worker_id":1,"action":"keep","reason":""}')
echo "$OUT" | grep -q '必须填写原因'
check "复核无原因→拒绝并要求留痕" $?

# 新合同不能追溯已结算月
OUT=$(curl -s -X POST "$BASE/api/contracts" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"worker_id":1,"day_rate":400,"effective_from":"2026-07"}')
echo "$OUT" | grep -q '不能追溯'
check "新单价追溯已结算月→拦截" $?

# 班组长/监理/工人不能结算、发薪、调价
for role_tok in "$SL:班组长" "$SP:监理" "$WK:工人"; do
  TOK=${role_tok%%:*}; NM=${role_tok##*:}
  OUT=$(curl -s -X POST "$BASE/api/payroll/settle" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d '{"month":"2026-09","confirm":true}')
  echo "$OUT" | grep -q '仅总包'
  check "$NM 无权结算" $?
done

# 未结算月可正常预览、导出
OUT=$(curl -s "$BASE/api/payroll?month=2026-09" -H "Authorization: Bearer $GC" -o /dev/null -w '%{http_code}')
[ "$OUT" = "200" ]
check "9月未结算实时预览 200" $?
curl -s "$BASE/api/attendance/export?view=month&month=2026-09&caliber=hours" -H "Authorization: Bearer $GC" | grep -q '工时口径'
check "出勤CSV表头写明工时口径" $?
curl -s "$BASE/api/payroll/export?month=2026-08" -H "Authorization: Bearer $GC" | grep -q '已发金额'
check "分账CSV含已发金额列" $?

echo ""
echo "通过 $PASS 项，失败 $FAIL 项"
[ $FAIL -eq 0 ]
