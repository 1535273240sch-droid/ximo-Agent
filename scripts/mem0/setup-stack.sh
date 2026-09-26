#!/usr/bin/env bash
# ============================================================================
#  setup-stack.sh —— 补齐 mem0 compose 栈起栈前缺失的两件本机配置
#
#  上游 server/ 只提供 .env.example，不提供 .env；而 docker-compose.yaml 里写了
#  `env_file: .env` 且 POSTGRES_PASSWORD 用了 ${...:?} 必填断言 ——
#  所以全新 clone 直接 `docker compose up -d` 一定失败（报 env file not found）。
#  本脚本做两件事，都只写进 mem0 检出目录（%MEM0_DIR%），不碰本仓库：
#
#    1) 从 .env.example 生成 server/.env，填入随机强 POSTGRES_PASSWORD 与
#       JWT_SECRET（值不打印到终端，只落在文件里）；
#    2) 生成 server/docker-compose.override.yaml，把发布端口绑到 127.0.0.1。
#       上游 compose 把 8888/3000/8432 发布在所有网卡上，而本地联调常用
#       AUTH_DISABLED=true，绑回环才不会把记忆接口暴露给局域网。
#       注意 override 里 ports 必须写 `!override`：Compose 的合并规则里 ports 是
#       「唯一资源」，唯一键是 {ip,target,published,protocol}（compose-spec
#       13-merge.md），只写 "127.0.0.1:8888:8000" 会与上游 "8888:8000" **并存**，
#       宿主机 8888 被绑两次。!override 需要 Compose v2.24.4+。
#
#  用法：
#    scripts/mem0/setup-stack.sh                  # 生成 .env（AUTH_DISABLED=true）+ 回环 override
#    AUTH_DISABLED=false MEM0_ADMIN_API_KEY=<随机串> scripts/mem0/setup-stack.sh
#                                                 # 开鉴权：往 .env 写 ADMIN_API_KEY
#    BIND=0.0.0.0 scripts/mem0/setup-stack.sh      # 不要回环限制：删掉本脚本生成过的 override
#    FORCE=1 scripts/mem0/setup-stack.sh           # 覆盖已存在的文件（会换掉 Postgres 密码！）
#
#  幂等：已存在且未被 FORCE 覆盖时不动原文件。
# ============================================================================
set -euo pipefail

have() { command -v "$1" >/dev/null 2>&1; }
tou()  { if have cygpath; then cygpath -u "$1"; else printf '%s' "$1"; fi; }

MEM0_DIR="${MEM0_DIR:-$(tou "$LOCALAPPDATA")/ximo-agent/third_party/mem0}"
SERVER_DIR="$MEM0_DIR/server"
BIND="${BIND:-127.0.0.1}"
AUTH_DISABLED="${AUTH_DISABLED:-true}"
FORCE="${FORCE:-0}"

if [ ! -d "$SERVER_DIR" ]; then
  printf '[FAIL] 找不到 mem0 检出：%s\n' "$SERVER_DIR" >&2
  printf '       先跑 scripts/mem0/mem0-up.cmd（它会 clone 并切到固定版本）。\n' >&2
  exit 1
fi
if [ ! -f "$SERVER_DIR/.env.example" ]; then
  printf '[FAIL] 缺少 %s/.env.example —— 检出可能不完整。\n' "$SERVER_DIR" >&2
  exit 1
fi

rand() {
  if have openssl; then openssl rand -hex "$1"
  else head -c "$(( $1 * 2 ))" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

ENV_FILE="$SERVER_DIR/.env"
if [ -f "$ENV_FILE" ] && [ "$FORCE" != "1" ]; then
  printf '[SKIP] %s 已存在（要覆盖设 FORCE=1；注意会换掉 Postgres 密码，需一并清数据卷）\n' "$ENV_FILE"
else
  PG_PW="$(rand 24)"
  JWT="$(rand 32)"

  # 以 .env.example 为模板，只替换需要本机化的键。
  awk -v pw="$PG_PW" -v jwt="$JWT" -v ad="$AUTH_DISABLED" -v adm="${MEM0_ADMIN_API_KEY:-}" '
    /^POSTGRES_PASSWORD=/ { print "POSTGRES_PASSWORD=" pw; next }
    /^JWT_SECRET=/        { print "JWT_SECRET=" jwt; next }
    /^AUTH_DISABLED=/     { print "AUTH_DISABLED=" ad; next }
    /^ADMIN_API_KEY=/     { print "ADMIN_API_KEY=" adm; next }
    { print }
  ' "$SERVER_DIR/.env.example" > "$ENV_FILE"

  if grep -q '^POSTGRES_PASSWORD=$' "$ENV_FILE" || grep -q '^JWT_SECRET=$' "$ENV_FILE"; then
    printf '[FAIL] .env 里仍有空值，请检查 .env.example 的键名\n' >&2
    exit 1
  fi
  if [ "$AUTH_DISABLED" = "true" ]; then
    printf '[WARN] AUTH_DISABLED=true —— 任何能访问该端口的人都能读写记忆，仅限本地开发\n'
    [ "$BIND" = "127.0.0.1" ] || printf '[WARN] BIND=%s 不是回环地址，配合 AUTH_DISABLED=true 会暴露到局域网\n' "$BIND"
  fi
  printf '[OK]   已生成 %s（POSTGRES_PASSWORD / JWT_SECRET 为随机值，未打印到终端）\n' "$ENV_FILE"
fi

# .env.example 里的 LLM/embedder 默认值指向 OpenAI；用别的服务商时改这里或事后 POST /configure。
grep -qE '^OPENAI_API_KEY=.+$' "$ENV_FILE" || \
  printf '        提示：%s 的 OPENAI_API_KEY 为空，先用 POST /configure 配好 LLM 与 embedder（见 verify.sh）\n' "$ENV_FILE"

OVERRIDE="$SERVER_DIR/docker-compose.override.yaml"
OVERRIDE_MARK='scripts/mem0/setup-stack.sh'
if [ "$BIND" = "127.0.0.1" ]; then
  if [ -f "$OVERRIDE" ] && [ "$FORCE" != "1" ]; then
    printf '[SKIP] %s 已存在\n' "$OVERRIDE"
  else
    cat > "$OVERRIDE" <<'YAML'
# 由 ximo-agent 的 scripts/mem0/setup-stack.sh 生成：把发布端口收回本机回环。
#
# ports 在 Compose 的合并规则里是「唯一资源」，唯一键是 {ip, target, published, protocol}
# （compose-spec 13-merge.md）。所以只写 "127.0.0.1:8888:8000" 并不会替换上游的
# "8888:8000" —— ip 不同即视为新条目，两条都会生效，宿主机 8888 被绑两次。
# 必须用 !override 才能真正替换（需要 Compose v2.24.4+）。
services:
  mem0:
    ports: !override
      - "127.0.0.1:8888:8000"
  mem0-dashboard:
    ports: !override
      - "127.0.0.1:3000:3000"
  postgres:
    ports: !override
      - "127.0.0.1:8432:5432"
YAML
    printf '[OK]   已生成 %s（8888 / 3000 / 8432 只绑 127.0.0.1，用 !override 替换上游端口）\n' "$OVERRIDE"
    printf '        起栈前可自检：cd %%MEM0_DIR%%/server && docker compose config --services\n'
  fi
elif [ -f "$OVERRIDE" ]; then
  # BIND 不是回环时必须把本脚本生成的 override 撤掉：否则上一轮回环绑定会继续
  # 生效，BIND=0.0.0.0 看起来"没作用"。只删自己生成的，别动用户手写的文件。
  if grep -qF "$OVERRIDE_MARK" "$OVERRIDE"; then
    rm -f "$OVERRIDE"
    printf '[OK]   已删除本脚本生成的 %s（BIND=%s，不再限制回环）\n' "$OVERRIDE" "$BIND"
  else
    printf '[WARN] %s 不是本脚本生成的，未动它；它可能覆盖了你要的端口绑定\n' "$OVERRIDE"
  fi
fi

printf '\n下一步：\n'
printf '  scripts/mem0/mem0-up.cmd                       # clone + 切版本 + 生成 .env + 起栈\n'
printf '  **REST API 在 http://127.0.0.1:8888**（dashboard 在 3000，它不代理 /memories、/search）\n'
printf '  浏览器打开 http://127.0.0.1:3000 走向导；本机 Windows 通常没有 make/lsof，\n'
printf '  所以不要指望上游 `make bootstrap`，admin key 走 dashboard 向导，\n'
printf '  或者按上面的 AUTH_DISABLED 约定免 key（仅限本地）。\n'
printf '  MEM0_ENDPOINT=http://127.0.0.1:8888 scripts/mem0/verify.sh   # 四步联调验证\n'
