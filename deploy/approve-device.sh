#!/usr/bin/env bash
# 批准桌面插件的设备码登录：建一个专用用户并 approve。
# 注意：不要用 UID 作变量名（bash 里是只读变量，赋值会静默失败）。
set -uo pipefail
BASE=http://127.0.0.1:8600
TOKEN=$(sed -n 's/^XIMO_GATEWAY_ADMIN_TOKEN=//p' /home/ubuntu/ximo-gateway/gateway.env)
A=(-H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json')
CODE="${1:?用法: approve-device.sh <用户码>}"
UNAME="desktop_${RANDOM}"

echo "== 建用户 $UNAME =="
U=$(curl -s -m 10 "${A[@]}" -X POST "$BASE/admin/users" \
  -d "{\"username\":\"$UNAME\",\"password\":\"Desktop!${RANDOM}${RANDOM}\",\"group_id\":\"desktop\"}")
USER_ID=$(printf '%s' "$U" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("id",""))')
echo "  user_id=$USER_ID"
[ -z "$USER_ID" ] && { echo "建用户失败：$U"; exit 1; }

echo "== 批准设备码 $CODE =="
R=$(curl -s -m 10 -o /tmp/approve.json -w "%{http_code}" "${A[@]}" \
  -X POST "$BASE/admin/device/approve" -d "{\"user_code\":\"$CODE\",\"user_id\":\"$USER_ID\"}")
echo "  http=$R body=$(head -c 200 /tmp/approve.json)"

echo "== 给这个用户额度 =="
curl -s -m 10 "${A[@]}" -X POST "$BASE/admin/quota/adjust" \
  -d "{\"user_id\":\"$USER_ID\",\"kind\":\"topup\",\"amount\":1000000,\"reason\":\"desktop onboarding\",\"idempotency_key\":\"desktop-$USER_ID\"}" \
  | python3 -c 'import sys,json;d=json.load(sys.stdin);print("  balance_after=",(d.get("ledger") or {}).get("balance_after"))'
echo "  用户名=$UNAME  user_id=$USER_ID"
