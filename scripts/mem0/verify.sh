#!/usr/bin/env bash
# ============================================================================
#  verify.sh —— mem0 联调验证（Git Bash）
#
#  两件事：
#    scripts/mem0/verify.sh            四步验证：服务活着 → 记忆库里有什么 →
#                                      手动检索（验 embedder）→ 真实对话与降级
#    scripts/mem0/verify.sh configure  通过 POST /configure 设置抽取用的 LLM 与
#                                      向量模型（需要 **admin** 角色的 key）
#
#  环境变量（都不进日志、不落盘；脚本从不打印 key 的值）：
#    MEM0_ENDPOINT   默认 http://127.0.0.1:8888
#                    —— 8888 是上游 compose 里 API 的宿主端口；
#                       3000 是 dashboard，它不代理 /memories、/search，指错了全是 404
#    MEM0_API_KEY    有网关鉴权时必填，作为 X-API-Key 发送
#                    （mem0 的 verify_auth 见到 Authorization: Bearer 会直接按 JWT
#                      解析并返回，不回退到 X-API-Key —— 用 Bearer 传 key 一定 401）
#    MEM0_USER_ID    默认 ximo-user，须与工作台 config.json 的 memory.user_id 一致
#    MEM0_QUERY      手动检索的查询词，默认「界面主题偏好」
#    MEM0_PROBE_TEXT 设了才做写入探针（内容由你提供，脚本不塞任何编造数据）
#
#  configure 用到的：MEM0_LLM_BASE_URL / MEM0_LLM_MODEL / MEM0_LLM_KEY
#                  MEM0_EMBED_BASE_URL / MEM0_EMBED_MODEL / MEM0_EMBED_KEY
#
#  退出码：0 = 四步都过；非 0 = 第几步失败就是几
# ============================================================================
set -uo pipefail

MEM0_ENDPOINT="${MEM0_ENDPOINT:-http://127.0.0.1:8888}"
MEM0_ENDPOINT="${MEM0_ENDPOINT%/}"
MEM0_API_KEY="${MEM0_API_KEY:-}"
MEM0_USER_ID="${MEM0_USER_ID:-ximo-user}"
MEM0_QUERY="${MEM0_QUERY:-界面主题偏好}"
MEM0_TOP_K="${MEM0_TOP_K:-10}"

case "$MEM0_ENDPOINT" in
  *:3000*)
    printf '[FAIL] MEM0_ENDPOINT 指向 3000 —— 那是 mem0 的 dashboard，不提供 REST API。\n' >&2
    printf '       上游 compose 里 API 在宿主机 8888（容器内 8000）。\n' >&2
    exit 1 ;;
esac

auth_args=()
[ -n "$MEM0_API_KEY" ] && auth_args=(-H "X-API-Key: $MEM0_API_KEY")

api() { # api METHOD PATH [BODY]
  local m="$1" p="$2" b="${3:-}"
  if [ -n "$b" ]; then
    curl -sS --max-time 60 -X "$m" "${auth_args[@]}" -H 'Content-Type: application/json' -d "$b" \
      -w '\n%{http_code}' "$MEM0_ENDPOINT$p" 2>&1
  else
    curl -sS --max-time 60 -X "$m" "${auth_args[@]}" -w '\n%{http_code}' "$MEM0_ENDPOINT$p" 2>&1
  fi
}

split_code() { printf '%s' "$1" | tail -1; }
split_body() { printf '%s' "$1" | sed '$d'; }
count_ids()  { printf '%s' "$1" | grep -o '"id"' | wc -l | tr -d ' '; }

# --------------------------------------------------------------- configure
if [ "${1:-}" = "configure" ]; then
  : "${MEM0_LLM_BASE_URL:?需要 MEM0_LLM_BASE_URL（例如 https://api.deepseek.com/v1）}"
  : "${MEM0_LLM_MODEL:?需要 MEM0_LLM_MODEL（例如 deepseek-chat）}"
  : "${MEM0_LLM_KEY:?需要 MEM0_LLM_KEY}"
  : "${MEM0_EMBED_BASE_URL:?需要 MEM0_EMBED_BASE_URL（DeepSeek 没有 embeddings 接口，必须另配）}"
  : "${MEM0_EMBED_MODEL:?需要 MEM0_EMBED_MODEL（例如 BAAI/bge-m3）}"
  : "${MEM0_EMBED_KEY:?需要 MEM0_EMBED_KEY}"

  # 上游 _validate_bundled_providers 的白名单：不在其中的 provider 一律 400。
  # 所以 Ollama / 硅基流动 / DashScope 都以 provider=openai + openai_base_url 接入。
  body=$(cat <<JSON
{"llm":{"provider":"openai","config":{"api_key":"$MEM0_LLM_KEY","model":"$MEM0_LLM_MODEL","openai_base_url":"$MEM0_LLM_BASE_URL"}},
 "embedder":{"provider":"openai","config":{"api_key":"$MEM0_EMBED_KEY","model":"$MEM0_EMBED_MODEL","openai_base_url":"$MEM0_EMBED_BASE_URL"}}}
JSON
)
  # 不把 body 回显（里面有 key），只回显端点与模型名。
  printf 'POST %s/configure  llm=%s@%s  embedder=%s@%s\n' \
    "$MEM0_ENDPOINT" "$MEM0_LLM_MODEL" "$MEM0_LLM_BASE_URL" "$MEM0_EMBED_MODEL" "$MEM0_EMBED_BASE_URL"
  out=$(api POST /configure "$body")
  code=$(split_code "$out")
  printf 'HTTP %s\n' "$code"
  printf '%s\n' "$(split_body "$out" | head -c 600)"
  case "$code" in
    200) printf '\n[OK] 已设置。注意：embedder 一旦换模型/换维度，pgvector 里已建的集合维度不匹配，\n     需要清库重建（docker compose down -v）或换一个新的 POSTGRES_COLLECTION_NAME。\n' ;;
    401|403) printf '\n[FAIL] 鉴权失败 —— POST /configure 需要 admin 角色（require_admin），普通 key 会被 403。\n' >&2 ;;
    400) printf '\n[FAIL] 400 见上面 detail：provider 不在白名单，或 config 形状不对。\n' >&2 ;;
    *)   printf '\n[FAIL] HTTP %s\n' "$code" >&2 ;;
  esac
  exit $(( code == 200 ? 0 : 1 ))
fi

step_fail=0
head1() { printf '\n== %s ==\n' "$1"; }
pass() { printf '  [OK]   %s\n' "$1"; }
fail() { printf '  [FAIL] %s\n' "$1"; step_fail=1; }

printf 'mem0 联调验证 —— endpoint=%s user_id=%s\n' "$MEM0_ENDPOINT" "$MEM0_USER_ID"
[ -n "$MEM0_API_KEY" ] && printf '鉴权：X-API-Key 已提供（值不打印）\n' || printf '鉴权：未提供 API Key（服务端须是 AUTH_DISABLED=true）\n'

# ------------------------------------------------------------- 第 1 步
head1 '第 1 步：服务与密钥都活着（GET /configure）'
out=$(api GET /configure); code=$(split_code "$out"); body=$(split_body "$out")
if [ "$code" = "200" ]; then
  pass 'HTTP 200'
  # 先在 "embedder" 处把文档切两半，再各自取 provider/model ——
  # 直接对整份 JSON 做贪婪匹配会把 embedder 的 model 当成 llm 的 model。
  llm_part=$(printf '%s' "$body" | sed 's/"embedder".*$//')
  emb_part=$(printf '%s' "$body" | sed 's/^.*\("embedder"\)/\1/')
  pick() { printf '%s' "$1" | sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1; }
  llm_p=$(pick "$llm_part" provider); llm_m=$(pick "$llm_part" model)
  emb_p=$(pick "$emb_part" provider); emb_m=$(pick "$emb_part" model)
  pass "llm.provider=${llm_p:-?}  model=${llm_m:-?}"
  pass "embedder.provider=${emb_p:-?}  model=${emb_m:-?}"
  [ "${emb_m:-text-embedding-3-small}" = "text-embedding-3-small" ] && \
    printf '         [WARN] embedder 仍是上游默认值 text-embedding-3-small（需要 OpenAI key）\n'
else
  fail "HTTP $code —— $(printf '%s' "$body" | head -c 200)"
  printf '         服务没起来就看：docker compose ps / docker compose logs --tail=50 mem0\n'
  printf '         401 说明开了鉴权而 key 不对；X-API-Key 是唯一可用的头。\n'
  printf '         连不上先确认端口是不是 8888（3000 是 dashboard）。\n'
  exit 1
fi

# ------------------------------------------------------------- 第 2 步
head1 '第 2 步：记忆库里有没有东西（GET /memories）'
out=$(api GET "/memories?user_id=$MEM0_USER_ID&top_k=$MEM0_TOP_K")
code=$(split_code "$out"); body=$(split_body "$out")
n=0
if [ "$code" = "200" ]; then
  n=$(count_ids "$body")
  pass "HTTP 200，返回 $n 条"
  if [ "$n" -eq 0 ]; then
    printf '         库里是空的。这一条**只有跑过真实对话**才可能有数据 ——\n'
    printf '         在并行的另一条线上跑一轮含信息量的对话，再重跑本步骤。\n'
  fi
else
  fail "HTTP $code —— $(printf '%s' "$body" | head -c 200)"
fi

# ------------------------------------------------------------- 第 3 步
head1 '第 3 步：手动检索（POST /search）—— 验 embedder'
out=$(api POST /search "{\"query\":\"$MEM0_QUERY\",\"top_k\":3,\"filters\":{\"user_id\":\"$MEM0_USER_ID\"}}")
code=$(split_code "$out"); body=$(split_body "$out")
if [ "$code" = "200" ]; then
  s=$(count_ids "$body")
  pass "HTTP 200，命中 $s 条（query=$MEM0_QUERY）"
  if [ "$n" -gt 0 ] && [ "$s" -eq 0 ]; then
    printf '\n  >>> 写进去了却召回不到 = **embedder 没配好**（02 文档第 2 节的判定表）。\n'
    printf '      DeepSeek 没有 embeddings 接口；用 scripts/mem0/verify.sh configure 重配。\n'
  fi
else
  fail "HTTP $code —— $(printf '%s' "$body" | head -c 200)"
fi

# ------------------------------------------------------------- 第 4 步
head1 '第 4 步：真实对话的写入与召回（需要工作台，脚本无法代替）'
cat <<'TXT'
  4a) 打开工作台，跑一轮有信息量的对话，例如「以后所有界面都用暗色主题」；
      对话正常结束即说明召回/回填都没有拖慢或打断它。
  4b) 重跑第 2 步，条数应当增加（这是写入生效的唯一证据）。
  4c) 再开一轮新对话，检查发给模型的请求里是否多出一条以
      "--- 长期记忆 (mem0) ---" 开头的 system 消息，位置在系统提示词之后、用户消息之前。
  4d) 降级也要验一次：把 mem0 停掉（scripts/mem0/mem0-down.cmd）再跑一轮 ——
      应当只有一条「长期记忆召回失败，本轮按「无记忆」继续」的告警，对话完全不受影响。
TXT
if [ -n "${MEM0_PROBE_TEXT:-}" ]; then
  printf '\n  写入探针（MEM0_PROBE_TEXT 已提供，内容进真实记忆库）：\n'
  # 转义双引号与反斜杠，免得探针文本里的引号把 JSON 弄坏、报成看不懂的 422。
  probe=$(printf '%s' "$MEM0_PROBE_TEXT" | sed 's/\\/\\\\/g; s/"/\\"/g')
  out=$(api POST /memories "{\"messages\":[{\"role\":\"user\",\"content\":\"$probe\"},{\"role\":\"assistant\",\"content\":\"$probe\"}],\"user_id\":\"$MEM0_USER_ID\",\"agent_id\":\"ximo-agent\"}")
  printf '  HTTP %s  %s\n' "$(split_code "$out")" "$(split_body "$out" | head -c 300)"
  printf '  回查：MEM0_PROBE_TEXT="..." scripts/mem0/verify.sh\n'
else
  printf '\n  （未做写入探针。要手工验证写入：MEM0_PROBE_TEXT="..." scripts/mem0/verify.sh\n'
  printf '    脚本不会替你编造内容 —— 塞进去的就是真的长期记忆。）\n'
fi

printf '\n'
[ "$step_fail" -eq 0 ] && printf '前 3 步全部通过。\n' || printf '有步骤失败，见上面 [FAIL]。\n'
exit $(( step_fail == 0 ? 0 : 2 ))
