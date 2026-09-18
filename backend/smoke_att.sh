#!/bin/bash
# 工瞳 API 冒烟测试 · 出勤打卡与分账
# 前置：服务已在 $BASE 启动并完成种子（含 2026-07 已发清 / 2026-08 部分已发冻结月）
set -u
BASE=http://localhost:7199
PASS=0; FAIL=0

check() {
  if [ "$2" = "0" ]; then echo "  ✅ $1"; PASS=$((PASS+1)); else echo "  ❌ $1"; FAIL=$((FAIL+1)); fi
}

login() {
  curl -s -X POST $BASE/api/login -H 'Content-Type: application/json' \
    -d "{\"username\":\"$1\",\"password\":\"gt123456\"}" | python3 -c "import sys,json;print(json.load(sys.stdin).get('token',''))"
}

GC=$(login gc_admin); SL=$(login sub_leader); SP=$(login supervisor); WK=$(login worker01); RG=$(login regulator)

# 取一个本月工作日（避开冻结月），用今天之后第 3 天并滚动到工作日
TESTDAY=$(python3 - <<'PY'
import datetime
d = datetime.date.today() + datetime.timedelta(days=3)
while d.weekday() >= 5:
    d += datetime.timedelta(days=1)
print(d.isoformat())
PY
)
echo "测试打卡日：$TESTDAY"
LASTMONTH=$(date -d "$TESTDAY -1 month" +%Y-%mm 2>/dev/null || python3 -c "import datetime;print((datetime.date.today().replace(day=1)-datetime.timedelta(days=1)).strftime('%Y-%m'))")
echo "上一冻结月：$LASTMONTH"

echo "== 1. 甘特与数据隔离 =="
curl -s "$BASE/api/attendance/gantt?view=week" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json; d=json.load(sys.stdin)
assert d.get('workers'), 'no workers'
assert 'days' in d['calibers'] and 'hours' in d['calibers']
assert d['calibers']['days']['text'].startswith('出勤率（按出勤天）')
assert d['calibers']['hours']['text'].startswith('出勤率（按打卡工时）')
assert 0 <= d['total']['rate_days'] <= 1 and 0 <= d['total']['rate_hours'] <= 1
print('ok')" >/dev/null 2>&1
check "总包甘特+双口径与口径文案" $?

curl -s "$BASE/api/attendance/gantt" -H "Authorization: Bearer $WK" | python3 -c "
import sys,json; d=json.load(sys.stdin)
assert len(d['workers'])==1 and d['workers'][0]['job_no']=='GT-1001', d.get('workers')
print('ok')" >/dev/null 2>&1
check "工人只见本人甘特(GT-1001)" $?

curl -s "$BASE/api/attendance/gantt" -H "Authorization: Bearer $SL" | python3 -c "
import sys,json; d=json.load(sys.stdin)
assert d['workers'] and all(w['team']=='宏宇劳务·钢筋一班' for w in d['workers']), [w['team'] for w in d['workers']]
print('ok')" >/dev/null 2>&1
check "班组长只见本班组甘特" $?

echo "== 2. 打卡来源与权限 =="
curl -s -X POST "$BASE/api/attendance/punch" -H "Authorization: Bearer $RG" -H 'Content-Type: application/json' \
  -d '{"worker_id":1,"punch_date":"'$TESTDAY'","punch_time":"07:50:00","source":"gate"}' | grep -q '只读'
check "监管员打卡被拒" $?

curl -s -X POST "$BASE/api/attendance/punch" -H "Authorization: Bearer $WK" -H 'Content-Type: application/json' \
  -d '{"worker_id":1,"punch_date":"'$TESTDAY'","punch_time":"07:50:00","source":"gate"}' | grep -q '手机定位'
check "工人闸机打卡被拒(仅手机定位)" $?

curl -s -X POST "$BASE/api/attendance/punch" -H "Authorization: Bearer $WK" -H 'Content-Type: application/json' \
  -d '{"worker_id":2,"punch_date":"'$TESTDAY'","punch_time":"07:50:00","source":"mobile"}' | grep -q '本人'
check "工人给他人打卡被拒" $?

echo "== 3. 多源合并（两台闸机 + 手机，同日只成一条） =="
OP="att-$(date +%s)"
for body in \
  "{\"worker_id\":1,\"punch_date\":\"$TESTDAY\",\"punch_time\":\"07:51:00\",\"source\":\"gate\",\"device\":\"东门闸机A\",\"client_op_id\":\"$OP-1\"}" \
  "{\"worker_id\":1,\"punch_date\":\"$TESTDAY\",\"punch_time\":\"07:51:00\",\"source\":\"gate\",\"device\":\"西门闸机B\",\"client_op_id\":\"$OP-2\"}" \
  "{\"worker_id\":1,\"punch_date\":\"$TESTDAY\",\"punch_time\":\"07:52:00\",\"source\":\"mobile\",\"device\":\"工人手机定位\",\"client_op_id\":\"$OP-3\"}" \
  "{\"worker_id\":1,\"punch_date\":\"$TESTDAY\",\"punch_time\":\"17:40:00\",\"source\":\"gate\",\"device\":\"东门闸机A\",\"client_op_id\":\"$OP-4\"}"; do
  curl -s -X POST "$BASE/api/attendance/punch" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' -d "$body" >/dev/null
done
# 同 client_op_id 重放 → duplicate
curl -s -X POST "$BASE/api/attendance/punch" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d "{\"worker_id\":1,\"punch_date\":\"$TESTDAY\",\"punch_time\":\"07:51:00\",\"source\":\"gate\",\"device\":\"东门闸机A\",\"client_op_id\":\"$OP-1\"}" \
  | grep -q '"duplicate":true'
check "同打卡重放幂等去重" $?

curl -s "$BASE/api/workers/1/attendance?view=month&month=${TESTDAY:0:7}" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
d=json.load(sys.stdin)
day=d['workers'][0]['days']['$TESTDAY']
assert day['total_punches'] >= 4, day
assert '闸机' in day['sources'] and '手机' in day['sources'], day['sources']
assert day['first_in'].startswith('07:51'), day
assert day['status'] in ('present','late'), day['status']
print('ok')" >/dev/null 2>&1
check "多源打卡合并为一条日结论(来源闸机+手机)" $?

echo "== 4. 作废 / 改判 =="
PID=$(curl -s "$BASE/api/workers/1/attendance?view=month&month=${TESTDAY:0:7}" -H "Authorization: Bearer $GC" \
  | python3 -c "import sys,json
for p in json.load(sys.stdin)['punches']:
    if p['date']=='$TESTDAY' and p['device']=='西门闸机B': print(p['id']); break")
[ -n "$PID" ]
check "找到西门闸机B流水" $?

curl -s -X POST "$BASE/api/attendance/punches/$PID/void" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"reason":""}' | grep -q '请填写'
check "作废无原因被拒(必填留痕)" $?

curl -s -X POST "$BASE/api/attendance/punches/$PID/void" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"reason":"冒烟：设备误识别作废"}' >/dev/null
curl -s "$BASE/api/workers/1/attendance?view=month&month=${TESTDAY:0:7}" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
d=json.load(sys.stdin)
p=[p for p in d['punches'] if p['id']==$PID][0]
assert p['voided'] is True
day=d['workers'][0]['days']['$TESTDAY']
assert day['total_punches']-day['valid_punches']==1, day
assert any(c['reason']=='冒烟：设备误识别作废' for c in d['corrections']), 'no correction log'
print('ok')" >/dev/null 2>&1
check "作废生效+日结论重算+留痕" $?

echo "== 5. 已结算冻结月：改判需二次确认，且不重算工资 =="
JULY=$(python3 -c "import datetime;print((datetime.date.today().replace(day=1)-datetime.timedelta(days=1)).strftime('%Y-%m'))")
# 上个月的某个工作日
LOCKDAY=$(python3 - <<PY
import datetime,calendar
y,m=map(int,'$JULY'.split('-'))
for day in range(10,16):
    d=datetime.date(y,m,day)
    if d.weekday()<5: print(d.isoformat()); break
PY
)
BEFORE=$(curl -s "$BASE/api/payroll/month?month=$JULY" -H "Authorization: Bearer $GC" | python3 -c "import sys,json;print(json.load(sys.stdin)['total_amount'])")

curl -s -X POST "$BASE/api/attendance/adjudicate" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"worker_id":1,"day":"'$LOCKDAY'","status":"absent","reason":"x"}' | python3 -c "
import sys,json; d=json.load(sys.stdin)
assert d.get('need_confirm') is True, d
print('ok')" >/dev/null 2>&1
check "改判冻结月首次→409 need_confirm" $?

curl -s -X POST "$BASE/api/attendance/adjudicate" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"worker_id":1,"day":"'$LOCKDAY'","status":"absent","reason":"冒烟：冻结月改判留痕","confirm":true}' | grep -q 'frozen_month_unchanged":true'
check "二次确认+原因后改判成功(标记不重算)" $?

AFTER=$(curl -s "$BASE/api/payroll/month?month=$JULY" -H "Authorization: Bearer $GC" | python3 -c "import sys,json;print(json.load(sys.stdin)['total_amount'])")
[ "$BEFORE" = "$AFTER" ]
check "冻结月工资总额未被重算($BEFORE == $AFTER)" $?

echo "== 6. 合同版本：新版本不影响已冻结月 =="
curl -s "$BASE/api/payroll/month?month=$JULY" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
d=json.load(sys.stdin)
l=[l for l in d['lines'] if l['job_no']=='GT-1001']
assert l and l[0]['daily_rate']==280, l
assert d['frozen'] is True
print('ok')" >/dev/null 2>&1
check "$JULY 冻结在原单价280(新320不回溯)" $?

curl -s "$BASE/api/payroll/month?month=${TESTDAY:0:7}" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
d=json.load(sys.stdin)
l=[l for l in d['lines'] if l['job_no']=='GT-1001']
assert (not l) or l[0]['daily_rate']==320, l
print('ok')" >/dev/null 2>&1
check "本月试算用新单价320" $?

echo "== 7. 部分发薪不被重算冲销 + 超额拦截 =="
AUG=$(python3 -c "import datetime;print(datetime.date.today().strftime('%Y-%m'))")
# 当月未结算，发薪应被拒（必须先结算）
curl -s -X POST "$BASE/api/payroll/pay" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"'$AUG'","worker_id":1,"amount":10}' | grep -q '尚未结算'
check "未结算月发薪被拒" $?

# 找上个月（已部分发）一个还有待发的人
PREV=$(python3 -c "import datetime;print((datetime.date.today().replace(day=1)-datetime.timedelta(days=1)).strftime('%Y-%m'))")
read WID REM <<< $(curl -s "$BASE/api/payroll/month?month=$PREV" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
d=json.load(sys.stdin)
for l in d['lines']:
    if l['remaining']>100: print(l['worker_id'], int(l['remaining'])); break")
[ -n "${WID:-}" ]
check "上月存在待发人员(wid=$WID)" $?

curl -s -X POST "$BASE/api/payroll/pay" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"'$PREV'","worker_id":'$WID',"amount":50,"note":"冒烟部分发"}' | grep -q '"ok":true'
check "部分发薪50成功" $?

curl -s -X POST "$BASE/api/payroll/pay" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"'$PREV'","worker_id":'$WID',"amount":999999}' | grep -q '超过待发'
check "超额发放被拦(已发不冲减)" $?

PAID=$(curl -s "$BASE/api/payroll/month?month=$PREV" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
l=[l for l in json.load(sys.stdin)['lines'] if l['worker_id']==$WID][0]
print(l['paid_amount'])")
python3 -c "exit(0 if float('$PAID')>=50 else 1)"
check "已发金额只增不减(paid=$PAID)" $?

echo "== 8. 差额调整不碰原快照 =="
FREEZE_AMT=$(curl -s "$BASE/api/payroll/month?month=$PREV" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
l=[l for l in json.load(sys.stdin)['lines'] if l['worker_id']==$WID][0]
print(l['amount'])")
curl -s -X POST "$BASE/api/payroll/adjust" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"'$PREV'","worker_id":'$WID',"amount":-999999,"reason":"x"}' | grep -q '低于已发'
check "扣减致应付低于已发被拒(已发不可冲销)" $?

curl -s -X POST "$BASE/api/payroll/adjust" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"'$PREV'","worker_id":'$WID',"amount":88.5,"reason":"冒烟：高温补差单列"}' | grep -q '"ok":true'
check "合法补差单列成功" $?

curl -s "$BASE/api/payroll/month?month=$PREV" -H "Authorization: Bearer $GC" | python3 -c "
import sys,json
l=[l for l in json.load(sys.stdin)['lines'] if l['worker_id']==$WID][0]
assert abs(l['amount']-$FREEZE_AMT)<0.01, l  # 原冻结金额不变
assert abs(l['adjustment']-88.5)<0.01, l
print('ok')" >/dev/null 2>&1
check "补差后原冻结快照金额不变" $?

echo "== 9. 结算冻结与重复结算 =="
curl -s -X POST "$BASE/api/payroll/settle" -H "Authorization: Bearer $SL" -H 'Content-Type: application/json' \
  -d '{"month":"'$AUG'"}' | grep -q '总包'
check "班组长不能结算" $?

curl -s -X POST "$BASE/api/payroll/settle" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"'$AUG'","note":"冒烟结算冻结"}' | grep -q '"ok":true'
check "当月结算冻结成功" $?

curl -s -X POST "$BASE/api/payroll/settle" -H "Authorization: Bearer $GC" -H 'Content-Type: application/json' \
  -d '{"month":"'$AUG'"}' | grep -q '已结算'
check "重复结算被拒(快照已冻结)" $?

echo "== 10. CSV 导出（口径随表写出） =="
curl -s "$BASE/api/attendance/export?view=month&month=${TESTDAY:0:7}" -H "Authorization: Bearer $GC" > /tmp/att.csv
grep -q '出勤率（按出勤天）= 实际出勤天 ÷ 应出勤天' /tmp/att.csv
check "CSV 含按天口径定义" $?
grep -q '出勤率（按打卡工时）= 有效打卡工时 ÷ 排班工时' /tmp/att.csv
check "CSV 含工时口径定义" $?
head -1 /tmp/att.csv | grep -q '出勤打卡导出'
check "CSV 表头与区间" $?

echo ""
echo "出勤/分账 通过 $PASS 项，失败 $FAIL 项"
[ $FAIL -eq 0 ]
