#!/usr/bin/env bash
# ximo-gateway 部署/更新脚本（在服务器上以 ubuntu 身份执行）。
# 幂等：重复执行只做「装服务 + 重启」，不会重新生成管理令牌。
set -euo pipefail

DIR=/home/ubuntu/ximo-gateway
ENV_FILE="$DIR/gateway.env"

if [ ! -f "$ENV_FILE" ]; then
  umask 077
  printf 'XIMO_GATEWAY_ADMIN_TOKEN=%s\nXIMO_GATEWAY_HOME=%s\n' "$(openssl rand -hex 32)" "$DIR" > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  echo "[1/5] 已生成管理令牌 -> $ENV_FILE（0600，内容未回显；查看：cat $ENV_FILE）"
else
  echo "[1/5] 沿用已存在的 $ENV_FILE（未改动令牌）"
fi

echo "[2/5] 安装 systemd 单元"
sudo cp "$DIR/ximo-gateway.service" /etc/systemd/system/ximo-gateway.service
sudo chmod 644 /etc/systemd/system/ximo-gateway.service
sudo systemctl daemon-reload

echo "[3/5] 启动（enable --now）"
sudo systemctl enable --now ximo-gateway >/dev/null
sleep 2

echo "[4/5] 状态与健康检查"
systemctl is-active ximo-gateway || true
curl -s -m 5 http://127.0.0.1:8600/v1/health || true
echo

echo "[5/5] 监听与最近日志"
ss -tln | grep -E ':8600' || echo "（未监听 8600，请看下面的日志）"
sudo journalctl -u ximo-gateway -n 14 --no-pager || true
