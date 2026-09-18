#!/bin/bash
# 工瞳 API 冒烟测试
set -u
BASE=${BASE:-http://localhost:7105}
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
echo "通过 $PASS 项，失败 $FAIL 项"
[ $FAIL -eq 0 ]
