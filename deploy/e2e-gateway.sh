#!/usr/bin/env bash
# 中转站部署后的端到端验证（在服务器上执行；管理令牌只在本机读取，不回显）。
#
# 覆盖：建用户 → 幂等充值 → 配 Provider（明文密钥只进 secretref）→ 建模型与映射 →
#       发 API Key → 列模型 → 补全（非流式/流式）→ 用量落库 → 账本对账 →
#       额度不足 402 → 未知模型 404
#
# 已知的客户端可见限制（不是计费缺口，见第 9 步）：网关不把 usage 分片转发给 SSE 客户端，
# 客户端要拿 token 数得查 /v1/usage。上游 usage 是否被正确解析，用服务端 usage 行来证。
set -uo pipefail

DIR=/home/ubuntu/ximo-gateway
BASE=http://127.0.0.1:8600
TOKEN=$(sed -n 's/^XIMO_GATEWAY_ADMIN_TOKEN=//p' "$DIR/gateway.env")
if [ -z "$TOKEN" ]; then echo "拿不到管理令牌，退出"; exit 1; fi

ADMIN=(-H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json')
SUF=$RANDOM
PASS=0; FAIL=0
ok()  { echo "  [PASS] $1"; PASS=$((PASS+1)); }
bad() { echo "  [FAIL] $1"; FAIL=$((FAIL+1)); }

jqf() {
  python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
for k in sys.argv[1].split("."):
    if d is None: break
    if isinstance(d, list):
        d = d[int(k)] if k.lstrip("-").isdigit() and 0 <= int(k) < len(d) else None
    elif isinstance(d, dict):
        d = d.get(k)
    else:
        d = None
print("" if d is None else d)
' "$1"
}
usage_count() { curl -s -m 10 -H "Authorization: Bearer $1" "$BASE/v1/usage?limit=50" \
  | python3 -c 'import sys,json;print(len(json.load(sys.stdin).get("data") or []))'; }

echo "== 1. 健康检查 =="
H=$(curl -s -m 5 "$BASE/v1/health")
[ "$(echo "$H" | jqf status)" = "ok" ] && ok "health ok" || bad "health: $H"

echo "== 2. 建用户 =="
U1=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/users" \
  -d "{\"username\":\"e2e_$SUF\",\"password\":\"E2ePass!$SUF\",\"group_id\":\"e2e\"}")
UID1=$(echo "$U1" | jqf id)
[ -n "$UID1" ] && ok "建用户 $UID1" || bad "建用户失败: $U1"
U2=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/users" \
  -d "{\"username\":\"poor_$SUF\",\"password\":\"PoorPass!$SUF\"}")
UID2=$(echo "$U2" | jqf id)
[ -n "$UID2" ] && ok "建未充值用户 $UID2" || bad "建用户失败: $U2"

echo "== 3. 充值（幂等：同 idempotency_key 提交两次） =="
KEY="topup-$SUF"
A1=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/quota/adjust" \
  -d "{\"user_id\":\"$UID1\",\"kind\":\"topup\",\"amount\":5000000,\"reason\":\"e2e\",\"idempotency_key\":\"$KEY\"}")
A2=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/quota/adjust" \
  -d "{\"user_id\":\"$UID1\",\"kind\":\"topup\",\"amount\":5000000,\"reason\":\"e2e\",\"idempotency_key\":\"$KEY\"}")
L1=$(echo "$A1" | jqf ledger.id); L2=$(echo "$A2" | jqf ledger.id)
BAL=$(echo "$A2" | jqf account.total_amount)
if [ "$L1" = "$L2" ] && [ "$BAL" = "5000000" ]; then ok "幂等充值：同账本行 $L1，余额 $BAL"
else bad "幂等充值失败：ledger $L1 vs $L2, total=$BAL"; fi

echo "== 4. 配 Provider（明文密钥必须变成 secretref） =="
P1=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/providers" \
  -d "{\"id\":\"fake$SUF\",\"name\":\"假上游\",\"endpoint\":\"http://127.0.0.1:8799/v1\",\"protocol\":\"openai-chat\",\"api_key\":\"sk-fake-upstream-$SUF\",\"status\":\"enabled\",\"weight\":10}")
PREF=$(echo "$P1" | jqf api_key_ref)
case "$PREF" in
  secretref:v1:*) ok "密钥已存为引用 $PREF" ;;
  *) bad "api_key_ref 不是 secretref: $P1" ;;
esac
echo "$P1" | grep -q "sk-fake-upstream" && bad "响应里回显了明文密钥！" || ok "响应未回显明文密钥"

echo "== 5. 建模型与映射 =="
M=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/models" \
  -d "{\"id\":\"e2e-model-$SUF\",\"display_name\":\"E2E 模型\",\"capabilities_json\":\"{\\\"stream\\\":true,\\\"tools\\\":true}\",\"enabled\":true}")
[ -n "$(echo "$M" | jqf id)" ] && ok "建模型" || bad "建模型失败: $M"
PM=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/providers/fake$SUF/models" \
  -d "{\"model_id\":\"e2e-model-$SUF\",\"upstream_model_id\":\"fake-large\",\"priority\":1,\"enabled\":true}")
echo "$PM" | grep -qi "error" && bad "建映射失败: $PM" || ok "建映射"

echo "== 6. 发用户 API Key =="
K=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/keys" -d "{\"user_id\":\"$UID1\"}")
UK=$(echo "$K" | jqf api_key)
case "$UK" in
  ximo_sk_*) ok "拿到用户密钥（掩码显示：${UK:0:12}…）" ;;
  *) bad "发 Key 失败: $K"; UK="";;
esac
K2=$(curl -s -m 10 "${ADMIN[@]}" -X POST "$BASE/admin/keys" -d "{\"user_id\":\"$UID2\"}")
UK2=$(echo "$K2" | jqf api_key)

echo "== 7. 列出模型（用户态） =="
ML=$(curl -s -m 10 -H "Authorization: Bearer $UK" "$BASE/v1/models")
echo "$ML" | grep -q "e2e-model-$SUF" && ok "模型列表含 e2e-model-$SUF" || bad "模型列表异常: $ML"

echo "== 8. 非流式补全 =="
T0=$(date +%s)
C1=$(curl -s -m 60 -H "Authorization: Bearer $UK" -H 'Content-Type: application/json' \
  -X POST "$BASE/v1/chat/completions" \
  -d "{\"model\":\"e2e-model-$SUF\",\"messages\":[{\"role\":\"user\",\"content\":\"你好\"}]}")
T1=$(date +%s)
CT=$(echo "$C1" | jqf choices.0.message.content)
IT=$(echo "$C1" | jqf usage.prompt_tokens); OT=$(echo "$C1" | jqf usage.completion_tokens)
if [ -n "$CT" ]; then ok "补全成功：$CT（tokens $IT/$OT，耗时 $((T1-T0))s）"
else bad "补全失败: $C1"; fi

echo "== 9. 流式补全 =="
BEFORE=$(usage_count "$UK")
S=$(curl -sN -m 60 -H "Authorization: Bearer $UK" -H 'Content-Type: application/json' \
  -X POST "$BASE/v1/chat/completions" \
  -d "{\"model\":\"e2e-model-$SUF\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"流式\"}]}")
echo "$S" | grep -q "\[DONE\]" && ok "SSE 以 [DONE] 收尾" || bad "流式缺 [DONE]"
echo "$S" | grep -qi '"error"' && bad "SSE 里出现错误分片" || ok "SSE 无错误分片"
AFTER=$(usage_count "$UK")
TOK=$(curl -s -m 10 -H "Authorization: Bearer $UK" "$BASE/v1/usage?limit=1" | jqf data.0.input_tokens)
if [ "$AFTER" -eq "$((BEFORE+1))" ] && [ "$TOK" = "11" ]; then
  ok "流式请求的服务端 usage 正确（尾随 usage 已被解析：input=$TOK）"
else
  bad "流式 usage 异常：before=$BEFORE after=$AFTER input=$TOK"
fi

echo "== 10. 用量与账本对账 =="
ACC=$(curl -s -m 10 "${ADMIN[@]}" "$BASE/admin/quota/accounts/$UID1")
AVAIL=$(echo "$ACC" | jqf available)
USED=$(echo "$ACC" | jqf used_amount)
LED=$(curl -s -m 10 "${ADMIN[@]}" "$BASE/admin/quota/ledger?user_id=$UID1&limit=200")
SUM=$(echo "$LED" | python3 -c '
import sys, json
d = json.load(sys.stdin); rows = d.get("data") or []
print(sum(int(r.get("amount") or 0) for r in rows))')
COST=$(curl -s -m 10 -H "Authorization: Bearer $UK" "$BASE/v1/usage?limit=50" | python3 -c '
import sys, json
d = json.load(sys.stdin); rows = d.get("data") or []
print(sum(int(r.get("cost_micro") or 0) for r in rows))')
echo "  账户：available=$AVAIL used=$USED ；账本 Σamount=$SUM ；用量 Σcost=$COST"
if [ "$AVAIL" = "$SUM" ] && [ "$AVAIL" = "$((5000000 - USED))" ]; then ok "账本与账户对账一致（available == Σamount == 充值 - used）"
else bad "对账不一致：available=$AVAIL Σamount=$SUM used=$USED"; fi
[ "$COST" -gt 0 ] && ok "计费非零（Σcost=$COST 微单位）" || bad "计费为 0（检查 --price-micro-per-ktok）"

echo "== 11. 额度不足 → 402（不应打到上游） =="
R=$(curl -s -m 20 -o /tmp/poor.json -w "%{http_code}" -H "Authorization: Bearer $UK2" \
  -H 'Content-Type: application/json' -X POST "$BASE/v1/chat/completions" \
  -d "{\"model\":\"e2e-model-$SUF\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}")
[ "$R" = "402" ] && ok "未充值用户被拒 402（$(head -c 90 /tmp/poor.json)）" || bad "期望 402，得到 $R: $(head -c 200 /tmp/poor.json)"

echo "== 12. 未知模型 → 404 =="
R2=$(curl -s -m 20 -o /tmp/nomodel.json -w "%{http_code}" -H "Authorization: Bearer $UK" \
  -H 'Content-Type: application/json' -X POST "$BASE/v1/chat/completions" \
  -d '{"model":"nope-does-not-exist","messages":[{"role":"user","content":"hi"}]}')
[ "$R2" = "404" ] && ok "未知模型 404" || bad "期望 404，得到 $R2: $(head -c 200 /tmp/nomodel.json)"

echo
echo "================ 汇总：PASS=$PASS FAIL=$FAIL ================"
[ "$FAIL" -eq 0 ]
