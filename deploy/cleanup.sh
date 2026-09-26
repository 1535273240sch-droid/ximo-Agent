#!/usr/bin/env bash
# 收尾：把验证时建的假 Provider / 假模型禁用掉（管理 API 没有删除接口，用 upsert 置为 disabled）。
set -uo pipefail
DIR=/home/ubuntu/ximo-gateway
BASE=http://127.0.0.1:8600
TOKEN=$(sed -n 's/^XIMO_GATEWAY_ADMIN_TOKEN=//p' "$DIR/gateway.env")
A=(-H "X-Admin-Token: $TOKEN" -H 'Content-Type: application/json')

echo "== 禁用所有 Provider（本机只有验证用的假上游） =="
curl -s -m 10 "${A[@]}" "$BASE/admin/providers" | python3 -c '
import sys, json
d = json.load(sys.stdin); rows = d.get("data") or []
for p in rows: print(p.get("id",""))
' | while read -r id; do
  [ -z "$id" ] && continue
  R=$(curl -s -m 10 "${A[@]}" -X POST "$BASE/admin/providers" -d "{\"id\":\"$id\",\"status\":\"disabled\"}")
  echo "  $id -> $(printf '%s' "$R" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status","?"))' 2>/dev/null)"
done

echo "== 禁用验证用的模型（id 以 e2e- 开头） =="
curl -s -m 10 "${A[@]}" "$BASE/admin/models" | python3 -c '
import sys, json
d = json.load(sys.stdin); rows = d.get("data") or []
for m in rows:
    mid = m.get("id") or m.get("model_id") or ""
    if mid.startswith("e2e-"): print(mid)
' | while read -r mid; do
  [ -z "$mid" ] && continue
  R=$(curl -s -m 10 "${A[@]}" -X POST "$BASE/admin/models" -d "{\"id\":\"$mid\",\"enabled\":false}")
  echo "  $mid -> enabled=$(printf '%s' "$R" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("enabled","?"))' 2>/dev/null)"
done

echo "== 现状 =="
curl -s -m 10 "${A[@]}" "$BASE/admin/providers" | python3 -c '
import sys, json
for p in json.load(sys.stdin).get("data") or []: print("  provider", p.get("id"), p.get("status"))
'
curl -s -m 10 "${A[@]}" "$BASE/admin/models" | python3 -c '
import sys, json
for m in json.load(sys.stdin).get("data") or []: print("  model", m.get("id"), "enabled=", m.get("enabled"))
'
echo "== 用户数（验证残留用户会保留，便于你查看账本） =="
curl -s -m 10 "${A[@]}" "$BASE/admin/users?limit=100" | python3 -c '
import sys, json
rows = json.load(sys.stdin).get("data") or []
print("  共", len(rows), "个用户：", ", ".join(u.get("username","") for u in rows[:12]))
'
