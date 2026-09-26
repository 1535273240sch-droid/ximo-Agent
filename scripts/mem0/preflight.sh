#!/usr/bin/env bash
# ============================================================================
#  preflight.sh —— 长期记忆（mem0）联调前置检查（Git Bash）
#
#  它回答一个问题：**这台机器现在能不能把 mem0 跑起来**，以及不能的话缺什么。
#  只读，不改任何东西；不打印任何密钥的值。
#
#  为什么需要它：mem0 的官方自托管栈是 docker compose（Postgres + pgvector +
#  FastAPI + dashboard），前置条件有好几层（Docker → WSL2 → 虚拟化 → 镜像仓库可达），
#  任何一层没到位，报出来的错都不是「缺前置」而是看似无关的 compose 报错。
#  这里把每一层都显式查一遍。
#
#  用法：  scripts/mem0/preflight.sh
#  退出码：0 = 没有任何 FAIL；1 = 存在 FAIL
# ============================================================================
set -uo pipefail

FAILS=0
WARNS=0

ok()   { printf '  [OK]   %s\n' "$1"; }
warn() { printf '  [WARN] %s\n' "$1"; WARNS=$((WARNS + 1)); }
bad()  { printf '  [FAIL] %s\n' "$1"; FAILS=$((FAILS + 1)); }
note() { printf '         %s\n' "$1"; }
head1() { printf '\n== %s ==\n' "$1"; }

have() { command -v "$1" >/dev/null 2>&1; }
tou()  { if have cygpath; then cygpath -u "$1"; else printf '%s' "$1"; fi; }

# Windows 可选功能状态（需要管理员/较慢；查不到就跳过，不算 FAIL）
optional_feature_state() {
  have powershell || return 1
  MSYS_NO_PATHCONV=1 powershell -NoProfile -Command \
    "(Get-WindowsOptionalFeature -Online -FeatureName $1).State" 2>/dev/null \
    | tr -d '\r' | tr -d '\000' | head -1
}

http_code() { curl -s -o /dev/null -w '%{http_code}' --max-time "${2:-15}" "$1" 2>/dev/null; }

printf 'XimoAgent 长期记忆（mem0）联调预检 —— %s\n' "$(date '+%Y-%m-%d %H:%M:%S')"

# ---------------------------------------------------------------- 1. 运行时
head1 '1. mem0 运行时（二选一：Docker 或 Python）'

if have docker; then
  if docker info >/dev/null 2>&1; then
    ok "docker 可用：$(docker --version 2>&1 | head -1)"
  else
    bad 'docker 已安装但 daemon 不可用 —— 打开 Docker Desktop 后确认 `docker info` 有输出'
  fi
else
  bad '找不到 docker（官方自托管栈必需）：winget install Docker.DockerDesktop'
fi

if have python && python --version >/dev/null 2>&1; then
  ok "python 可用：$(python --version 2>&1)"
else
  warn '找不到可用的 python（Python 库路线才需要；官方自托管栈不需要）'
fi

if have make; then
  ok 'make 可用（mem0 的 `make bootstrap` 需要它）'
else
  warn '找不到 make —— mem0 的 `make bootstrap` 用不了，只能 `docker compose up -d`，'
  note '首个 admin API Key 要改从 dashboard 向导或 scripts/seed.sh 拿'
fi

# ------------------------------------------------- 2. Docker 在 Windows 的前置
head1 '2. Docker Desktop 在 Windows 上的前置（WSL2 / 虚拟化 / 镜像仓库）'

if have wsl; then
  # wsl.exe 在没有发行版时把**用法说明**打到 stdout 而退出码非 0，
  # 所以不能按行数判断（会误报几十个「发行版」），必须看退出码。
  # wsl.exe 输出是 UTF-16-ish 且带 NUL，交给 tr 在管道里清掉（否则 bash 会告警）。
  distro_out=$(MSYS_NO_PATHCONV=1 wsl.exe -l -q 2>/dev/null | tr -d '\000\r'); distro_rc=${PIPESTATUS[0]}
  if [ "$distro_rc" -eq 0 ] && [ -n "$(printf '%s' "$distro_out" | tr -d '[:space:]')" ]; then
    cnt=$(printf '%s\n' "$distro_out" | grep -c '[^[:space:]]')
    ok "WSL 已有 ${cnt} 个发行版"
  else
    bad 'WSL 没有任何可用发行版 —— Docker Desktop 需要 WSL2 后端'
    note '需要管理员权限并重启：wsl --install'
  fi
else
  bad '找不到 wsl.exe'
fi

for f in VirtualMachinePlatform Microsoft-Windows-Subsystem-Linux Microsoft-Hyper-V-All; do
  st=$(optional_feature_state "$f" || true)
  case "${st:-}" in
    Enabled)  ok "$f = Enabled" ;;
    Disabled) bad "$f = Disabled（启用需要管理员 + 重启）" ;;
    *)        warn "$f 状态未知（查询需要管理员权限）" ;;
  esac
done

for u in https://registry-1.docker.io/v2/ https://auth.docker.io/token; do
  c=$(http_code "$u")
  case "$c" in
    000|'') bad "不可达：$u（拉取官方镜像会失败）" ;;
    *)      ok "可达：$u（HTTP $c）" ;;
  esac
done
for u in https://docker.m.daocloud.io/v2/ https://cdn.jsdelivr.net https://pypi.org/simple/; do
  c=$(http_code "$u")
  case "$c" in
    000|'') warn "不可达：$u" ;;
    *)      ok "可达：$u（HTTP $c）" ;;
  esac
done

# ------------------------------------------------------------- 3. 向量模型
head1 '3. 向量模型（embedder）—— 这一条不满足的症状是「写得进、召回不到」'

if have ollama; then
  ok "本机已有 ollama：$(ollama --version 2>&1 | head -1)"
  if ollama list 2>/dev/null | grep -qi 'embed'; then
    ok 'ollama 里已有 embedding 模型'
  else
    warn 'ollama 里没有 embedding 模型：ollama pull nomic-embed-text'
  fi
else
  warn '本机没有 ollama；要么装它（ollama.com 可达），要么用云端 embeddings 服务'
fi
note 'DeepSeek 没有 embeddings 接口，不能当 embedder；抽取用的 LLM 才可以用 DeepSeek'
note '注意 mem0 服务端的 /configure 只接受 provider ∈ (openai, gemini)，'
note '所以 Ollama / 硅基流动 / DashScope 都要以 provider=openai + openai_base_url 的方式接'

# --------------------------------------------------------- 4. mem0 检出与栈
head1 '4. mem0 检出与 compose 栈'

MEM0_DIR="${MEM0_DIR:-$(tou "$LOCALAPPDATA")/ximo-agent/third_party/mem0}"
if [ -d "$MEM0_DIR/server" ]; then
  ok "mem0 检出存在：$MEM0_DIR"
  if [ -f "$MEM0_DIR/server/.env" ]; then
    ok 'server/.env 已存在'
    for k in POSTGRES_PASSWORD JWT_SECRET; do
      if grep -qE "^${k}=.+" "$MEM0_DIR/server/.env"; then
        ok "  .env 里 ${k} 非空"
      else
        bad "  .env 里 ${k} 为空 —— compose 起不来（POSTGRES_PASSWORD 是 :? 必填）"
      fi
    done
  else
    bad 'server/.env 不存在 —— 上游 compose 声明了 env_file: .env，缺它 `docker compose up -d` 直接失败'
    note '先跑：scripts/mem0/setup-stack.sh'
  fi
else
  warn "mem0 尚未检出：$MEM0_DIR（scripts/mem0/mem0-up.cmd 会 clone 它）"
fi

# -------------------------------------------------------- 5. 工作台配置
head1 '5. 工作台 config.json 的两个开关'

BASE_DIR="${XIMO_HOME:-$(tou "$APPDATA")/ximo-agent}"
CFG="$BASE_DIR/config.json"
if [ -f "$CFG" ]; then
  ok "配置文件存在：$CFG"
  if grep -qE '"memory[.]mem0"[[:space:]]*:[[:space:]]*true' "$CFG"; then
    ok '一级开关 feature_flags["memory.mem0"] = true'
  else
    warn '一级开关 feature_flags["memory.mem0"] 不是 true（整机熔断，记忆不会生效）'
  fi
  if grep -qE '"enabled"[[:space:]]*:[[:space:]]*true' "$CFG"; then
    ok '二级开关 memory.enabled = true'
  else
    warn '二级开关 memory.enabled 不是 true'
  fi
  ep=$(sed -n 's/.*"endpoint"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CFG" | head -1)
  if [ -n "$ep" ]; then
    ok "memory.endpoint = $ep"
    case "$ep" in
      *:3000*) bad '端口 3000 是 mem0 的 **dashboard**，不是 REST API —— /memories、/search 会 404' ;;
    esac
    case "$ep" in
      *:8888*) ok '端口 8888 是上游 compose 里的 API 端口（正确）' ;;
      *:8000*) warn '8000 是容器内端口；宿主机映射出来的是 8888' ;;
    esac
  else
    warn 'config.json 里没有 memory.endpoint'
  fi
  if grep -qE '"write_back"[[:space:]]*:[[:space:]]*false' "$CFG"; then
    warn 'write_back = false：只读不写（省略该键 = 默认开启）'
  fi
else
  warn "配置文件不存在：$CFG（工作台还没写过配置，记忆开关也就没地方开）"
fi

# ----------------------------------------------------------------- 结论
head1 '结论'
if [ "$FAILS" -eq 0 ]; then
  printf '  预检通过（%d 条 WARN）。按 scripts/mem0/README.md 起栈，\n' "$WARNS"
  printf '  起完跑 scripts/mem0/verify.sh 做四步联调验证。\n'
else
  printf '  %d 条 FAIL、%d 条 WARN。当前状态：**跑不起来**。\n' "$FAILS" "$WARNS"
  printf '  先解决上面的 FAIL；其中 Windows 上最关键的是 WSL2 + 虚拟化（需管理员 + 重启）。\n'
fi
printf '\n  mem0 侧必需的两样东西（与机器无关，只能由你提供）：\n'
printf '    1) 抽取用的 LLM：可用 DeepSeek，但要能被 mem0 当作 provider=openai + openai_base_url 调用\n'
printf '    2) 向量模型：DeepSeek 没有 embeddings 接口，必须另配（Ollama / 硅基流动 / DashScope）\n'
printf '  两者都用 POST %s/configure 设置，且需要 **admin** 角色的 key。\n' "${MEM0_ENDPOINT:-http://127.0.0.1:8888}"

exit $(( FAILS > 0 ? 1 : 0 ))
