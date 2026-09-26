# scripts/mem0 —— 长期记忆后端的安装与启停

本目录只负责一件事：把 [mem0](https://github.com/mem0ai/mem0)（Apache-2.0，长期记忆层）
**按固定版本装到本机并起停它**。记忆如何接入 Go 引擎、怎么配置、怎么验证，见
[`docs/长期记忆-mem0.md`](../../docs/长期记忆-mem0.md)。

| 文件 | 作用 | 运行环境 |
|---|---|---|
| `mem0-up.cmd` | 检出 mem0 → 生成 `server/.env` 与回环 override → 起栈 | cmd（Windows） |
| `mem0-down.cmd` | 停栈；`--purge` 连数据卷一起删 | cmd（Windows） |
| `setup-stack.sh` | 生成 `server/.env`（随机强口令）与回环 override，唯一实现 | Git Bash |
| `preflight.sh` | 只读预检：这台机器现在能不能跑起来、缺什么 | Git Bash |
| `verify.sh` | 四步联调验证 + `configure` 子命令设 LLM 与 embedder | Git Bash |

## 端口（最容易记错的地方）

| 端口 | 是什么 | 谁用 |
|---|---|---|
| **8888** | mem0 REST API（容器内 8000） | 工作台 `memory.endpoint` 写这个 |
| 3000 | dashboard（给人看的界面） | 浏览器；**它不代理 `/memories`、`/search`**，指错全是 404 |
| 8432 | Postgres（pgvector） | 只用来排查，应用不连它 |

三个端口默认只绑 `127.0.0.1`（见下面「回环绑定」）。本机实测默认控制台代码页是 936，
cmd 脚本里已经把 8888/3000/8432 写进启动与排查提示，不要再改回 3000。

## 本机为什么起不来（按顺序补）

预检脚本会把下面每一条都查一遍：

```bash
bash scripts/mem0/preflight.sh          # 只读；0 = 无 FAIL，1 = 有 FAIL
```

当前这台机器的实测状态（2026-09-26，结论：**跑不起来**）：

| 层 | 现状 | 怎么补 | 需要你授权/重启吗 |
|---|---|---|---|
| 1. Docker 运行时 | 没有 `docker` | `winget install Docker.DockerDesktop` | **要**：官方安装器会请求提权 |
| 2. Windows 前置 | `VirtualMachinePlatform`、`Microsoft-Windows-Subsystem-Linux`、`Microsoft-Hyper-V-All` 三个可选功能都是 **Disabled**；`wsl -l -q` 无任何发行版 | 管理员 PowerShell：`wsl --install`，然后 `dism /online /enable-feature /featurename:VirtualMachinePlatform /all /norestart` 等 | **要**：管理员 + **重启一次**，之后才谈得上 Docker |
| 3. 镜像仓库 | `registry-1.docker.io` / `auth.docker.io` **不可达**（curl 000）；`docker.m.daocloud.io` 可达 | Docker Desktop → Settings → Docker Engine 里配 `registry-mirrors` | 不要（但装完必须配，否则拉不到 pgvector 镜像） |
| 4. 向量模型 key | 本机没有 Ollama，也没有任何 embeddings 服务商 key | 三选一：装 Ollama 并 `ollama pull nomic-embed-text`；或硅基流动 `BAAI/bge-m3`；或 DashScope `text-embedding-v3` | 不要，但**必须由你提供 key**（脚本不替你申请） |
| 5. 抽取用的 LLM key | 无 | DeepSeek 即可（`provider=openai` + `openai_base_url`） | 不要，同样需要你提供 |
| 6. 上游检出 | 不存在 | `scripts\mem0\mem0-up.cmd` 会 clone；github.com 不通时见下面「取不到源码怎么办」 | 不要 |
| 7. 工作台配置 | `%APPDATA%\ximo-agent\config.json` 不存在 | 工作台写过配置后，把 `memory.enabled` 与 `feature_flags["memory.mem0"]` 都开成 `true` | 不要 |

**第 2 层是最容易被忽略的一层**：WSL2 与虚拟化没启用时，Docker Desktop 装上也起不来，
而报出来的错看起来与 Docker 无关。这一步必须重启，脚本替你做不了。

## 起停

```cmd
REM 第一次：检出 + 生成 .env + 起栈（首次要构建镜像，十几分钟）
scripts\mem0\mem0-up.cmd

REM 先干跑一遍：只查前置并生成 .env，不 clone、不起栈（没有 docker 时也能看缺什么）
set MEM0_UP_DRYRUN=1
scripts\mem0\mem0-up.cmd

REM 指定另一个版本
scripts\mem0\mem0-up.cmd v3.0.0

REM 停栈（保留记忆）
scripts\mem0\mem0-down.cmd

REM 停栈并清空记忆（数据卷一起删）
scripts\mem0\mem0-down.cmd --purge
```

实际检出的 commit 会写到 `%LOCALAPPDATA%\ximo-agent\third_party\mem0-revision.txt`
（非 git 检出时跳过这一步）。

## `.env`：全新机器起不来的真正原因

上游 `server/` 只提供 `.env.example`，而它自己的 `docker-compose.yaml` 里写了：

```yaml
    env_file:
      - .env
    environment:
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD:?Set POSTGRES_PASSWORD in .env}
```

`env_file: .env` 缺文件 → `docker compose up -d` 直接报 **env file not found**；
`${:?}` 是必填断言 → 值空也不起。所以 clone 完必须有一步生成 `server/.env`，
`mem0-up.cmd` 的第 3 步就是它，实现在 `setup-stack.sh`（只有这一份，脚本不重写第二遍）。

生成时**用随机强口令填 `POSTGRES_PASSWORD` 与 `JWT_SECRET`，值不打印到终端**，
只落在 `%MEM0_DIR%\server\.env` 里。想看它值用编辑器打开，不要让脚本回显。

一些取舍：

- **`AUTH_DISABLED`**：脚本默认写 `true`（上游默认是 `false`）。这样本地起完就能直接用，
  代价是任何能访问该端口的人都能读写记忆 —— 所以端口必须绑回环（下一节）。
  要用真鉴权：`AUTH_DISABLED=false MEM0_ADMIN_API_KEY=<随机串> bash scripts/mem0/setup-stack.sh`。
- **`FORCE=1`** 会重写 `.env` 并换掉 Postgres 口令，而数据卷里的库还是老口令，
  必须同时 `docker compose down -v`（或 `scripts\mem0\mem0-down.cmd --purge`）。
- mem0 的 `/configure` 只接受 `provider ∈ {openai, gemini}`，而默认 embedder 是
  `text-embedding-3-small`（要 OpenAI key）。**不配 embedder 的症状是「写得进、召回不到」**，
  所以 `verify.sh` 第 3 步专门验这个。

## 回环绑定，以及为什么 `ports` 必须写 `!override`

上游 compose 把 8888/3000/8432 发布在**所有网卡**上。本地是 `AUTH_DISABLED=true`，
所以 `setup-stack.sh` 会生成 `server/docker-compose.override.yaml` 把它们收回
`127.0.0.1`。这里有个坑，写下来免得下一个人再踩：

> Compose 的合并规则里 **`ports` 是「唯一资源」，唯一键是 `{ip, target, published, protocol}`**
> （compose-spec `13-merge.md`）。所以只写 `"127.0.0.1:8888:8000"` **不会替换**上游的
> `"8888:8000"` —— ip 不同即视为新条目，两条都生效，宿主机 8888 被绑两次。
> 要真正替换必须加 `!override`，而它需要 **Compose v2.24.4+**。

`mem0-up.cmd` 因此在起栈前跑一次 `docker compose config --quiet` 兜底：失败时按
「提到 `!override` 就是 Compose 太老 / 提到 env file 就是 `.env` 没生成」给对照表，
而不是把 compose 的原始报错甩给你。不要回环限制时用 `BIND=0.0.0.0 bash scripts/mem0/setup-stack.sh`
（它会把本脚本生成过的 override 删掉，手写的文件不动）。

## 起栈、验证、配置

```bash
# 起完先看服务与配置（不需要 key 的部署可省 MEM0_API_KEY）
bash scripts/mem0/preflight.sh
MEM0_ENDPOINT=http://127.0.0.1:8888 scripts/mem0/verify.sh

# 设 LLM 与 embedder（POST /configure，需要 admin 角色的 key）
MEM0_ENDPOINT=http://127.0.0.1:8888 \
MEM0_LLM_BASE_URL=https://api.deepseek.com/v1 MEM0_LLM_MODEL=deepseek-chat MEM0_LLM_KEY=... \
MEM0_EMBED_BASE_URL=https://api.siliconflow.cn/v1 MEM0_EMBED_MODEL=BAAI/bge-m3 MEM0_EMBED_KEY=... \
bash scripts/mem0/verify.sh configure
```

`verify.sh` 的第 4 步（真实对话写入与降级）**脚本没法替你跑**，它只打印该怎么做。
`X-API-Key` 是唯一可用的鉴权头；用 `Authorization: Bearer` 一定 401。

## 取不到源码怎么办（github.com 不通）

本机实测 `git ls-remote https://github.com/mem0ai/mem0.git` 被 reset，
而 `cdn.jsdelivr.net`、`codeload.github.com` 可达。所以 clone 失败时可以改走 tarball：

```bash
REV=94c3fe9f238f3dbf29c9ce98643bd71eb13077cd
curl -sL --retry 5 -o mem0.tar.gz "https://codeload.github.com/mem0ai/mem0/tar.gz/$REV"
mkdir -p "$LOCALAPPDATA/ximo-agent/third_party/mem0"
tar -xzf mem0.tar.gz -C "$LOCALAPPDATA/ximo-agent/third_party/mem0" --strip-components=1
bash scripts/mem0/setup-stack.sh          # mem0-up.cmd 也认这种非 git 检出
```

注意 `codeload` 的 tarball 约 65MB，本机实测速度很慢（十几 KB/s 量级），要有耐心。

## 这台机器上没验证到的部分

- **真 compose 起栈、`!override` 的真实合并、pgvector 迁移**：本机没有 docker，
  一次都没跑过。上面 `!override` 的结论来自 compose-spec 与实测的 CRLF/端口行为，
  不是从真 compose 观察到的。
- `mem0-up.cmd` / `mem0-down.cmd` 的行为是用 **Go 写的 docker/git/where 替身**驱动验证的
  （33 项断言全过），替身能证明脚本逻辑与参数，**证明不了 mem0 自己**。
- `verify.sh` 只被 REST 替身验过，见 `../../docs/长期记忆-mem0.md`。

## 改这些 `.cmd` 前先读（踩过的坑）

1. **必须 CRLF 行尾**。LF-only 的 `.cmd` 会被 cmd.exe 逐行错位解析。
2. **必须纯 ASCII，连 echo 的中文都要去掉**。非 ASCII 字节会让解析器的字节偏移漂移：
   注释行的尾巴被当命令执行、整条语句被静默吞掉 —— 看起来像一个「正常跑完但少做了一步」
   的脚本。中文说明留在本文件（`.md` 不被 cmd 解析）。
3. **`REM` 行与 `echo` 文本里不能出现命令分隔符或重定向符**（`&`、`|`、`<`、`>`）。
   `&` 曾让脚本自己递归调用自己；两个 `<` 曾把整行变成一次失败的重定向，
   输出里只剩 "The system cannot find the file specified"。
4. **`chcp 65001` 之前先取旧代码页**，退出时还回去，别把用户的 cmd 会话改成 65001。
5. 别在批处理里用 `.cmd` 写替身：从批处理调用另一个 `.cmd` 而不加 `call` 会转移控制权、
   不返回，把被测脚本直接「吃掉」。替身要用 exe。
