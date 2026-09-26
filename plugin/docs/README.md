# ximo-plugin —— 把中转站接进本机任意 Agent

`ximo-plugin` 是一个**独立、通用**的命令行工具：把任意 **OpenAI 兼容 / Anthropic 兼容**的中转站
接进本机已经装好的 Agent（Claude Code、Codex CLI、Continue、Cursor、aider、XIMO Agent 等）。

它是**独立 Go module**（`github.com/1535273240sch-droid/ximo-plugin`），产物是**单个静态二进制**，
不依赖 ximo-Agent 运行时，也不 import 主仓库的任何包。你可以只把二进制拷到任何机器上用。

> **本文档的证据标注**（很重要，别混）：
> - ✅ **实跑** = 在 Windows 上用真二进制 + 真 `ximo-gateway` 服务端跑过，输出是从终端原样摘的。
> - 📖 **静态** = 只读代码 / 规格 JSON 得到的结论，**没有实跑**。
>
> 凡是没跑过的都标了 📖，不要把 📖 当成 ✅。

---

## 1. 内置了哪些适配器

适配器全部是**纯数据**（一份 JSON = 一个 Agent），跑 `ximo-plugin adapters list` 看全部。

| id | 名称 | 改什么 | 具体落点 |
| --- | --- | --- | --- |
| `claude-code` | Claude Code | 环境变量 | `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` |
| `codex-cli` | OpenAI Codex CLI | 环境变量 | `OPENAI_BASE_URL` / `OPENAI_API_KEY` |
| `continue` | Continue | 环境变量 | `OPENAI_BASE_URL` / `OPENAI_API_KEY` |
| `cursor` | Cursor | 环境变量 | `OPENAI_BASE_URL` / `OPENAI_API_KEY` |
| `aider` | aider | 环境变量 | `OPENAI_API_BASE` / `OPENAI_API_KEY` |
| `env-openai` | 仅环境变量（OpenAI 兼容） | **不落盘**，只打印语句 | 任意吃 `OPENAI_BASE_URL`/`OPENAI_API_KEY` 的客户端 |
| `env-anthropic` | 仅环境变量（Anthropic 兼容） | **不落盘**，只打印语句 | 任意吃 `ANTHROPIC_*` 的客户端 |
| `openai-compatible-generic` | 通用 OpenAI 兼容客户端 | 环境变量 + dotenv 文件 | `~/.ximo-plugin/openai-compatible.env` |
| `ximo-agent` | XIMO Agent（本产品） | **优先 IPC 帧**，回退改文件 | `%APPDATA%/ximo-agent/config.json` 的 `provider.{base_url,model,name,id}` |

实跑确认（✅）的清单与上面一致（9 个，`adapters list` 逐行核对过）。

### 两条必须知道的事

1. **除 `openai-compatible-generic` 与 `ximo-agent` 外，其余内置适配器都只走环境变量，不改任何文件。**
   这是**刻意的**：我们没有核实这些工具配置文件（`~/.claude/settings.json`、`~/.codex/config.toml`、
   `~/.continue/config.yaml`、Cursor 的内部存储、`~/.aider.conf.yml`）的字段 schema，
   猜错字段会破坏别人的配置文件。每个规格的 `description` 里都写明了「路径/schema 需用户确认」——
   **以 `specs/*.json` 的 `description` 为准**。
2. `~/.ximo-plugin/openai-compatible.env` **不是任何第三方客户端的官方配置路径**，
   它是本插件自己维护的 dotenv 落盘文件，里面是**明文 key**（0600）。不想落盘就只跑 `print-env`。

---

## 2. 安装与构建

需要 Go 1.27+（本仓库实测 `go1.27.1 windows/amd64`）。

```bash
cd plugin
export GOROOT=/c/ximo-tools/go && export PATH=$GOROOT/bin:$PATH && export GOPROXY=https://goproxy.cn,direct

go build ./...            # 整模块编译
go vet ./...              # 静态检查
go test ./... -count=1    # 本模块很小，可以全量跑
```

### 交叉编译成单文件二进制

```bash
# Windows amd64（本机）
go build    -o ximo-plugin.exe            ./cmd/ximo-plugin

# Linux / macOS / Windows-on-ARM：改 GOOS/GOARCH 即可
GOOS=linux   GOARCH=amd64 go build -o ximo-plugin-linux-amd64          ./cmd/ximo-plugin
GOOS=darwin  GOARCH=arm64 go build -o ximo-plugin-darwin-arm64         ./cmd/ximo-plugin
GOOS=windows GOARCH=arm64 go build -o ximo-plugin-windows-arm64.exe    ./cmd/ximo-plugin
```

产物路径：默认输出到**当前目录**（上例即 `plugin/` 下），用 `-o` 指定到别处。
装到 PATH 里（Linux/macOS）：`install -m 0755 ximo-plugin-linux-amd64 ~/.local/bin/ximo-plugin`。

✅ **实跑**：`go build ./... && go vet ./...` 全绿；四个平台都交叉编译成功，
`file` 校验为 `ELF 64-bit LSB executable … statically linked`（Linux）与
`Mach-O 64-bit arm64 executable`（macOS ARM）。Linux 产物是**静态链接**的，拷过去就能跑。

---

## 3. 快速开始

一条完整链路：`detect` → `login` → `models` → `apply --dry-run` → `apply --yes` → `doctor`。
下面每步都是**实跑输出摘录**（✅）。

> 全局 flag：`--home <dir>`（覆盖用户主目录，便于隔离测试）、`--json`、`--no-color`。
> 退出码：`0` 成功 / `1` 运行失败 / `2` 用法错误（✅ 实测了三类）。
> 下文用 `<GW>` 代表你的网关，例 `http://127.0.0.1:8600`。

### 3.1 `detect` —— 本机有哪些 Agent

```bash
ximo-plugin detect          # 只看命中的
ximo-plugin detect --all    # 连未命中的证据一起看
```

真实输出（✅，在一台装过 XIMO Agent 的 Windows 上）：

```
检测到 1 个可接入的 Agent：
[+] XIMO Agent（本产品） (ximo-agent)
      miss binary ximo-agent  [PATH 上未找到]
      miss path   %APPDATA%/ximo-agent/config.json -> C:\Users\...\AppData\Roaming/ximo-agent/config.json  [The system cannot find the file specified.]
      hit  path   %APPDATA%/ximo-agent -> C:\Users\...\AppData\Roaming/ximo-agent
      ...
（另有 8 个适配器未命中，用 `ximo-plugin detect --all` 看证据）
```

`detect` 的语义是 **any-of**：`binaries`（PATH 上找得到）/ `paths`（`~`、`%VAR%` 展开后存在）/
`env`（已设置的环境变量）**任一命中即算检测到**。每行证据都标 `hit`/`miss`，含展开后的真实路径。

### 3.2 `login` —— 设备码登录

```bash
ximo-plugin login --gateway <GW>
```

真实输出（✅ 对着真网关跑的完整设备码链路）：

```
网关：http://127.0.0.1:8611
用户码：C4CKQZM9
请在网关的授权页输入上面的用户码（V1 服务端不返回授权页地址）
等待授权（每隔 5s 轮询一次，上限 10m0s，Ctrl+C 可中断）…
PASS 登录成功：access token gwa_****2c9e
凭据已保存到 C:\Users\...\.ximo-plugin\cred.json（0600；P-Client 原子写）
```

拿到用户码后，需要在网关侧授权（管理员调 `POST /admin/device/approve`，或用户的授权页）。
凭据落在 `<home>/.ximo-plugin/cred.json`（0600），里面只有网关地址与令牌，
**终端只打印掩码**（`gwa_****2c9e`）。`access token` 过期时会自动用 `refresh_token` 续期。

### 3.3 `models` —— 看网关有哪些模型

```bash
ximo-plugin models --gateway <GW>
```

```
网关 http://127.0.0.1:8611 提供 1 个模型：
  - e2e-model (E2E Model)  provider=e2e-provider  protocols=openai-chat
```

`--json` 输出的是网关 `/v1/models` 的原始结构（`id`/`display_name`/`provider`/`protocols`/
`capabilities`/`enabled`）。

> 没有凭据时会明确报错，不会静默跳过（✅）：
> `错误: gateway: 本地没有可用凭据：…\cred.json 不存在，请先登录；也可用 --api-key 直接指定密钥`
> —— 这时可以用 `--api-key <k>`；**该值只在进程内存里，不落盘**。

### 3.4 `apply --dry-run` —— 先看差异，再决定写不写

```bash
ximo-plugin apply --gateway <GW> --spec openai-compatible-generic --dry-run
```

真实输出（✅）：

```
WARN 适配器 openai-compatible-generic 在本机没有检测到证据，仍按 --spec 继续。
目标网关：http://127.0.0.1:8611 （dry-run）以下变更不会被写入
[openai-compatible-generic] 通用 OpenAI 兼容客户端
    新建 C:\Users\...\.ximo-plugin\openai-compatible.env  设置 2 个字段：OPENAI_BASE_URL, OPENAI_API_KEY
    环境变量 OPENAI_BASE_URL OPENAI_API_KEY  需设置 2 个环境变量（本插件不落盘）
    重启提示：环境变量形式需重开终端；dotenv 文件由使用方自行 source，改完要重新 source 才会生效。
--- C:\Users\...\.ximo-plugin\openai-compatible.env (dry-run，未写入)
--- C:\Users\...\.ximo-plugin\openai-compatible.env (current)
+++ C:\Users\...\.ximo-plugin\openai-compatible.env (after)
@@ -0,0 +1,2 @@
+OPENAI_API_KEY=ximo********cdef
+OPENAI_BASE_URL=http://127.0.0.1:8611/v1
汇总：PASS 1 / FAIL 0
```

✅ 确认 `--dry-run` **不落盘**（该次跑完目标文件不存在）。
✅ 确认差异的**两侧凭据都被掩码**（`-` 侧的旧值与 `+` 侧的新值，都只保留前 4 / 后 4 位）。
   确实需要看真值时用 `--show-secrets`——那会把明文打进终端，别重定向进日志。

`--spec <id>` 与 `--all` 二选一；`--all` 只作用于 `detect` 命中的适配器。
`--model <m>` 省略且目标规格确实需要 model 时，会自动从网关挑一个（✅ 实测会打印
`未指定 --model，已从网关挑选：e2e-model`）；目标规格不需要 model 时不会去问网关。

### 3.5 `apply --yes` —— 真正写入

```bash
ximo-plugin apply --gateway <GW> --spec openai-compatible-generic --yes
```

```
WARN 适配器 openai-compatible-generic 在本机没有检测到证据，仍按 --spec 继续。
目标网关：http://127.0.0.1:8611 将执行以下变更
[openai-compatible-generic] 通用 OpenAI 兼容客户端
    新建 C:\Users\...\.ximo-plugin\openai-compatible.env  设置 2 个字段：OPENAI_BASE_URL, OPENAI_API_KEY
    ...
PASS [openai-compatible-generic] 已写入配置（原文件备份为 *.bak-<unix>）
汇总：PASS 1 / FAIL 0
```

- **不加 `--yes` 时会交互确认**：输入 `yes` 才继续；只回别的字符就取消（退出码 1，不改文件）。
  非交互终端（管道、脚本）下**不会傻等**，直接报错要求 `--yes` 或 `--dry-run`（✅）。
- 覆盖已有文件前会**备份成 `<path>.bak-<unix>`**（✅ 实测第二次 apply 生成了
  `openai-compatible.env.bak-1790397698`）。
- 写入是**原子写 + 保留未知字段**：只改规格声明的那几个字段，文件里其它键/其它段落原样保留。
- 写失败会**回滚本轮已写的文件**。

### 3.6 `doctor` —— 逐项体检

```bash
ximo-plugin doctor --gateway <GW>
```

真实输出（✅）：

```
PASS 适配器规格：9 个（home=C:/Users/.../temp/xp/home）
PASS 本机 Agent 检测：1 个：ximo-agent
WARN 凭据文件：Windows 不支持 POSIX 权限位：无法验证凭据文件为 0600（已尽力按最小权限创建）；真实访问控制由所在用户目录的 ACL 提供，请不要把凭据文件放到共享目录
WARN ximo-agent IPC：\\.\pipe\ximo-agent-ipc 不可达（…）；将走配置文件回退路径
PASS 网关健康：http://127.0.0.1:8611 status=ok version=v1.0.0-alpha
PASS 网关模型：1 个可用模型
```

**只有 `FAIL` 会让退出码变 1**；`WARN` 不影响退出码（✅ 实测上面这次含 2 个 WARN 仍返回 0）。
`doctor` 不带 `--gateway` 也能跑，此时网关相关项记 WARN 并跳过（✅）。

失败长这样（✅ 对着一个没开的端口跑，`exit=1`）：

```
FAIL 网关健康：http://127.0.0.1:9999：请求 GET /v1/health 失败: Get "http://127.0.0.1:9999/v1/health": dial tcp 127.0.0.1:9999: connectex: No connection could be made because the target machine actively refused it.
WARN 网关模型：无可用凭据（gateway: 凭据属于其它网关地址：…\cred.json 属于 http://127.0.0.1:8611，本次要访问 http://127.0.0.1:9999（请用对应的 --gateway 或重新登录）），已跳过
```

`--json` 时输出 `{"items":[{"name","status","detail"},…],"ok":false}` 并同样返回 1（✅ 实测）。
注意最后那条 WARN 体现了一个**安全保护**：凭据文件记着「它属于哪个网关」，
用它去访问**另一个网关会直接拒绝**，避免把 A 网关的令牌发给 B（✅ 实测）。

### 3.7 `print-env` —— 只打印环境变量语句

`env-*` 形态不落盘，用它生成脚本。三种风格（✅ 三种都实测）：

```bash
ximo-plugin print-env --gateway <GW> --spec env-anthropic --model <m> --style export
```

```
export ANTHROPIC_BASE_URL='http://127.0.0.1:8611/v1'
export ANTHROPIC_AUTH_TOKEN='<你的 key>'
```

`--style set` 出 `set OPENAI_BASE_URL=…`（Windows cmd）；`--style powershell` 出
`$env:OPENAI_BASE_URL="…"`。不指定 `--style` 时按规格声明，否则 Windows 用 `set`、其它平台用 `export`。

⚠️ `print-env` 的用途就是**把凭据交给你的 shell**，所以它一定会打印真实 key。
**不要把它的输出贴到聊天/工单/日志里。**

其他命令：`adapters list|show <id>`（看规格）、`usage --gateway <GW> [--limit N]`（看用量）。

---

## 4. 如何新增一个 Agent

**加一个新 Agent = 加一份 JSON，不用改代码，也不用重新编译插件。**

### 4.1 三步

1. 从最像的内置规格复制一份：
   - 只认环境变量 → 抄 `claude-code`（或 `env-openai`）；
   - 要改配置文件 → 抄 `openai-compatible-generic`（有 `files`）或 `ximo-agent`。
2. 存成 `~/.ximo-plugin/specs/<你的 id>.json`，改 `id` / `name` / `detect` / `files[].path` 与字段映射。
   **文件名必须与 `id` 完全一致**（`my-cli.json` ↔ `"id": "my-cli"`），否则加载直接报错。
3. 验一遍：

```bash
ximo-plugin adapters list                       # 应看到你的 id
ximo-plugin adapters show <你的 id>              # 逐字段回显
ximo-plugin detect                              # 证据路径对不对
ximo-plugin apply --gateway <GW> --spec <你的 id> --dry-run   # 先看差异
```

### 4.2 完整可用的最小示例（配置文件形态）

把下面存成 `~/.ximo-plugin/specs/my-cli.json`（或 `my-cli-file.json`，两边必须同名）。

**环境变量形态**：

```json
{
  "id": "my-cli",
  "name": "My CLI",
  "description": "示例：换成真实描述；路径/schema 需用户确认。",
  "detect": { "binaries": ["my-cli"], "paths": ["~/.my-cli"] },
  "env": { "base_url": "OPENAI_BASE_URL", "api_key": "OPENAI_API_KEY", "style": "export" },
  "restart_note": "需重启终端与 my-cli 进程。",
  "protocols": ["openai-chat"]
}
```

**配置文件形态**（会真的写文件，带数组下标映射）：

```json
{
  "id": "my-cli-file",
  "name": "My CLI（配置文件）",
  "description": "示例：目标文件为 ~/.my-cli/config.json，schema 需用户确认。",
  "detect": { "paths": ["~/.my-cli/config.json"] },
  "files": [
    { "path": "~/.my-cli/config.json", "format": "json", "create": true,
      "fields": [
        { "path": "providers.0.base_url", "from": "base_url" },
        { "path": "providers.0.api_key", "from": "api_key" },
        { "path": "model", "from": "model" }
      ] }
  ],
  "restart_note": "改完重开 my-cli。",
  "protocols": ["openai-chat"]
}
```

✅ **实跑**：这两份原样放进 `~/.ximo-plugin/specs/` 后，`adapters list` 从 9 个变成 **11 个**
并列出 `my-cli` / `my-cli-file`；`apply --spec my-cli-file --model e2e-model --dry-run` 生成了
正确的 JSON 差异；`--yes` 真的写出了文件（内容见下，key 已掩码）：

```json
{
  "model": "e2e-model",
  "providers": [
    { "api_key": "<redacted>", "base_url": "http://127.0.0.1:8611/v1" }
  ]
}
```

### 4.3 字段速查

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `id` | ✅ | 小写 slug；**必须与文件名相同** |
| `name` / `description` | ✅ | `description` 里 schema 不确定就写明「需用户确认」 |
| `detect` | ✅ | `binaries` / `paths` / `env`，**any-of**；`paths` 支持 `~`、`%VAR%`、`$VAR` |
| `env` | | `base_url`/`api_key`/`model` 填**变量名**（不是值！）；`extra` 只提示不赋值；`style` ∈ `export`/`set`/`powershell` |
| `files` | | `format` ∈ `json`/`toml`/`yaml`/`dotenv`；`create:false` 时文件不存在就报错而不是新建 |
| `restart_note` | ✅ | 改完要不要重启 |
| `protocols` | | `openai-chat` / `anthropic-messages` |

`files[].fields[].path` 是**点号路径**，数字段表示**数组下标**（例 `providers.0.base_url`）。
`from` ∈ `base_url` / `api_key` / `model` / `display_name` / `gateway_url` / `literal`；
`literal` 必须配 `value`，其它取值不允许带 `value`。

✅ `--home` 生效时家目录相关的环境变量一起隔离：`~`、`%APPDATA%`、`%LOCALAPPDATA%`、
`%USERPROFILE%`、`$HOME`、`$XDG_CONFIG_HOME`、`$XIMO_HOME` 全部按隔离后的 home 解析
（✅ 实测：`--home` 指向临时目录时，`ximo-agent.json` 的 `%APPDATA%/ximo-agent/config.json`
解析成 `<临时目录>\AppData\Roaming\ximo-agent\config.json`，不再落到真实用户目录）。
不给 `--home` 时这些变量仍按真实环境解析，此时规格里混用 `%APPDATA%` 与 `%XIMO_HOME%`
可能指向不同位置，以哪个为准要在规格里挑一个。

### 4.4 覆盖内置 / 排错

- **同名 ID 整体替换内置规格**（不做字段级合并）。
- 校验很严格，下面这些都**在校验期直接报错**（✅ 逐条实跑验证过，错误信息一次列全）：

  | 写错的地方 | 真实报错 |
  | --- | --- |
  | 文件名与 `id` 不一致 | `文件名必须与 id 一致（文件名 "mismatch"，id "wrong-id"）` |
  | 多写/拼错字段名 | `JSON 解析失败（注意：一个文件只能是一个 Spec 对象，字段名与契约 §2 必须一致）: json: unknown field "bogus_field"` |
  | `env.api_key` 里塞了 key 字面量 | `env.api_key="sk-literal-abc123" 必须是环境变量名而不是值` |
  | `path` 不是绝对路径 | `files[0].path: 路径 "rel/config.json" 必须是绝对路径` |
  | 既无 `files` 也无 `env` | `files 为空且 env.base_url / env.api_key 都未提供：该规格没有任何可执行动作` |

用 `--json` 拿结构化输出，方便脚本消费。

---

## 5. 安全与边界

### 设计上就是这么做的（✅ 已验证）

- **凭据 0600**：`<home>/.ximo-plugin/cred.json` 以 0600 原子写入（先建临时文件 → chmod → rename），
  目录 0700。终端/日志只出现**掩码**（`gwa_****2c9e`）。
  Windows 上 POSIX 权限位表达不了访问控制，`doctor` 会据此报 WARN 并提示真实保护来自用户目录 ACL。
- **不把明文 key 写进第三方 Agent 的配置**：内置规格里**没有任何**第三方 Agent 的
  `files[].fields[]` 映射到 `api_key` 或 `secret_ref`（逐份核对过，`grep '"from": "api_key"'`
  只命中一条）。`ximo-agent` 的规格**刻意不映射** `secret_ref`，走 IPC 时 key 进平台安全存储，
  配置里只留引擎回填的 `secret_ref`。
  唯一映射 `api_key` 的是 `openai-compatible-generic`，落点是**本插件自己维护的** dotenv 文件
  （不是任何第三方客户端的官方路径），且**只在用户显式 `--yes` 时才写**。
- **改配置前备份 + `--dry-run`**：覆盖前生成 `<path>.bak-<unix>`；`--dry-run` 不落盘；写失败回滚。
- **保留未知字段**：只改规格声明的路径，其它键原样保留（用 dot path 在解析出的文档上改，不是整体覆盖结构体）。
- **不改别人的东西**：内置适配器里除 `ximo-agent` 外**都不写第三方配置文件**——
  没核实 schema 就不猜，宁可只给环境变量。这是本工具的核心安全取舍。

### 你需要自己守的边界

- **只接入你有授权的服务**。本工具只负责把地址和凭据写进本机 Agent 配置，
  它**不绕过**上游的认证、限流、计费或任何服务条款。
- **不要用它去接别人没给你权限的中转站**，也不要把凭据文件放到共享目录或同步盘。
- 网关的额度/计费是网关侧的责任；`ximo-plugin usage` 只是**查**用量，不会修改任何账目。

### ⚠️ 已知风险与已修项

以下四条都曾在本项目中**实跑复现**，现已修复（保留记录，便于判断旧版本的行为）：

1. ~~`apply --dry-run` 的 `-`（current）一侧回显目标文件已有的明文凭据~~ → **已修**：
   差异的**两侧**都掩码（字段名词表 + 已知值替换）；`--show-secrets` 才显示真值。
2. ~~没有长期密钥时会写 `access token`（`gwa_…`，寿命约 900 秒）~~ → **已修**：
   默认**拒绝**写入短寿命令牌并给出指引（改用 `--api-key <长期 key>`）；只有显式
   `--allow-short-lived-token` 才放行，且会打印醒目告警。`doctor` 会检测 Agent 配置里
   是否被写入了短寿命令牌并报 FAIL。
3. ~~`create:true` 不会创建父目录~~ → **已修**：写入前自动补建父目录（0700）。
4. ~~`apply --all` 对"只有环境变量"的规格报 `PASS 已写入配置`，实际一个文件都没写~~ → **已修**：
   现在按规格的 `style` 打印可直接执行的环境变量行，结果标注为「已输出环境变量（未写入文件）」，
   汇总里单独计数，不再谎报"已写入"。

仍需自己注意：
- 环境变量形态永远要你 `export` / 重开终端——插件不会替你持久化进 shell 配置。
- Windows 上无法用权限位验证 0600/0700，`doctor` 会报 WARN（实际保护来自用户目录 ACL）。

---

## 6. 已知限制（照实写）

### 6.1 哪些 Agent 的字段名**未经真实安装验证**

下面这些适配器的 `description` **自己就写了**「schema 需用户确认」，本节只是集中复述
（出处：`plugin/internal/spec/specs/*.json` 的 `description`，📖）。**它们当前都只走环境变量，
不改文件**，所以「字段名未经核实」暂时不会破坏任何文件：

| id | 未核实的东西 | 当前做法 |
| --- | --- | --- |
| `claude-code` | `~/.claude/settings.json` 的字段 schema | 只用 `ANTHROPIC_BASE_URL` / `ANTHROPIC_AUTH_TOKEN` |
| `codex-cli` | `~/.codex/config.toml` 的 `model_providers` 段 schema | 只用通用 `OPENAI_*` |
| `continue` | `~/.continue/config.yaml`(新) / `config.json`(旧) 的 schema | 只用通用 `OPENAI_*` |
| `cursor` | Cursor 编辑器内部存储（VS Code 布局，非明文 JSON） | 只用通用 `OPENAI_*`；明确说"不可安全编辑" |
| `aider` | `~/.aider.conf.yml` 的完整 schema | 只用 `OPENAI_API_BASE` / `OPENAI_API_KEY` |
| `openai-compatible-generic` | 目标客户端的真实配置路径 | 只写**本插件自己的** dotenv |

**唯一**字段名对着真实代码核对过的文件形态是 `ximo-agent`
（`provider.{base_url,model,name,id}`，对照 `internal/config/config.go` 的 `ProviderConfig`，📖 静态核对）。
注意 `ximo-agent.json` 的默认路径写死为 Windows 的 `%APPDATA%/ximo-agent/config.json`；
若本机设了 `XIMO_HOME` 或在 macOS/Linux，**apply 前必须改 `files[0].path`**，否则会写错位置。

### 6.2 不支持的形态

- **`ximo-agent` 的 IPC 成功路径**：`internal/agents` 走 Windows 命名管道
  `\\.\pipe\ximo-agent-ipc`，需要 ximo-agent 正在运行。真进程联调时本机**没有运行中的
  ximo-agent**，实测到的是「IPC 不可达 → 回退配置文件」这条路径（退避提示正确）。
  帧兼容的成功路径由测试用**进程内假 agent** 覆盖（`internal/agents` 的 fakeagent，
  cmd 侧 `ipc_test.go` 用 `--ipc-endpoint tcp://…` 跑通 apply）：不再整体回传
  `providers` / `mcp_servers`、写前备份、dry-run 只读、安全存储不可用时逐条告警。
  → 仍**未**与真的 ximo-agent 进程跑过（那台机器上没有它）。
- **非 Windows 的默认端点为空**：`defaultIPCEndpoint()` 在非 Windows 返回空串，此时
  `ximo-agent` 走配置文件回退；要在这些平台上走 IPC，得显式给 `--ipc-endpoint`
  （支持 `tcp://host:port` 与 unix socket）。
- **`--dry-run` 会只读地连一次 IPC**（`system.config.get` + 模型列表：不写安全存储、
  不改配置、不落盘），所以能看到 IPC 侧的真实差异；连不上就告警并回退到文件差异。
- **`print-env` 不落盘、也不设置环境变量**：它只是**打印**，你得自己 `eval` / `source` / 贴进 shell。
- **不做**：插件市场 / 清单安装 / 多 provider 轮换 / 凭据写进系统钥匙串以外的后端 /
  自动热重载 Agent。V1 就是「写配置 + 打印环境变量」。
- **`env` 字段里的 `extra` 只提示，不会被赋值**。

### 6.3 需要重启的 Agent

改环境变量对**已经跑着的进程无效**，必须重启。各规格 `restart_note` 的原文口径（📖）：

| Agent | 重启要求 |
| --- | --- |
| `claude-code` | 关闭并重开终端与 Claude Code 会话；**运行中的会话读不到新值** |
| `codex-cli` | 重启终端与 `codex` 进程 |
| `continue` | 重启 IDE 窗口（Reload Window） |
| `cursor` | **完全退出 Cursor 再启动**（Reload Window 不够：环境变量只在进程启动时读） |
| `aider` | 重开终端 / 重新 source 配置 |
| `env-openai` / `env-anthropic` | 重启终端与目标客户端 |
| `openai-compatible-generic` | 环境变量要重开终端；dotenv 要重新 `source` |
| `ximo-agent` | 改 `config.json` 后**必须重启**；走 IPC 帧则**即时生效、无需重启** |

### 6.4 当前的工程状态（写文档时的实况，别当成已完成）

本模块正在被**多个代理并行开发**，测试状态在本次会话中变过几次。截至写完本文档时：

- ✅ `go build ./... && go vet ./...` **全绿**。
- ✅ `internal/formats`、`internal/gateway`、`internal/spec`、`internal/agents` 的测试**通过**
  （`internal/agents` 一度是红的，会话中被修好）。
- ❌ 只剩 `cmd/ximo-plugin` 的测试**当前是红的**
  （新增的 `TestExitCodes`、`TestApplyDryRunDoesNotWriteAndMasksKey`、`TestApplyYesWritesAndBacksUp`）。
  该包属于另外的代理（P-Core），本文档作者**未改动**它。`internal/engine` 一度也是红的，随后转绿。
- ⚠️ 注意：**命令跑不通 ≠ 二进制坏了**。本次验证用的二进制是用当时的工作树现编的，
  §3 里所有 ✅ 输出都来自**实际执行**，与单元测试的红绿无关。
- 上面的红灯意味着：**目前不要宣称本模块"测试全绿"**；重新跑一遍 `go test ./... -count=1` 再下结论。

> **追记（U-Repo 复核，接手写本仓库门面时实跑）**：上面这段已经**过时**。同一工作树上
> `gofmt -l .` 无输出，`go build ./... && go vet ./...` 全绿，`go test ./... -count=1` **7 个包全部 ok**
> （`cmd/ximo-plugin`、`internal/{agents,engine,formats,gateway,spec,ui}`）。上面提到的
> `cmd/ximo-plugin` 红灯已被修复；原文保留，以免抹掉当时的实况。

本文档所有 ✅ 结论的复现方式（已跑过）：`plugin/` 下 `go build ./...` 编出
`ximo-plugin.exe`，另编一个真 `ximo-gateway` 服务端（`cmd/ximo-gateway`，临时 `--db` + `--admin-token`），
用 `--home` 指向临时目录隔离，逐条跑 §3 的命令并核对输出。

---

## 7. 本地界面（UI）

`ximo-plugin` 自带一个**本地网页界面**，把 CLI 的流程串成一条可视化链路：
检测 → 连接网关 → 选模型 → 看差异 → 应用 → 体检 → 适配器。
它不是 Electron：静态资源用 `//go:embed assets/*` 编进**同一个单文件二进制**，
运行期零外部请求（不加载任何 CDN / 网络字体 / 远程图片），不需要 Node 或任何运行时。
设计口径见项目内的 UI 契约（`recon/插件UI契约-冻结.md`，独立仓库不随附该文件）。

### 7.1 启动

```bash
ximo-plugin ui                  # 默认 127.0.0.1:8787，尽力自动打开浏览器
ximo-plugin ui --no-open        # 只打印带令牌的地址，不自动打开
ximo-plugin ui --port 8899      # 指定端口（--port 0 = 让系统分配一个空闲端口）
ximo-plugin ui --gateway <GW>   # 预填网关地址；省略时用已保存凭据里的网关
ximo-plugin ui --home <dir>     # 全局 flag：隔离用户主目录（与 CLI 一致，便于测试）
```

（✅ 实跑核对：flag 取自 `cmd/ximo-plugin/ui.go` 的 FlagSet——`--port` 默认 `8787`、
`--no-open`、`--gateway`；`--home` 是全局 flag。`ximo-plugin --help` 的命令表里已有
`ui [--port 8787] [--no-open] [--gateway <url>]`。注意 `-h/--help` 是**全局** flag，
所以 `ximo-plugin ui --help` 打印的是全局用法，不是 ui 子命令的 flag 表。）

行为与边界：

- **只绑回环** `127.0.0.1`，绝不绑 `0.0.0.0`。
- **端口被占用直接报错**并提示 `ximo-plugin ui --port <其它端口>`，**不静默换端口**
  （你以为是 8787、实际跑在别的端口，比直接失败更难查）。
- 每次启动用 `crypto/rand` 生成 **32 位十六进制**会话令牌，地址形如
  `http://127.0.0.1:8787/?t=<token>`。**该地址等价于一次性访问凭证**，别贴进日志/工单/聊天记录；
  `--no-open` 会把完整地址打印给你，自动打开成功时只提示一次、不重复打印。
- `/api/*` 全部要求 `X-UI-Token`，缺失/不匹配返回 **401**（防止本机其它程序驱动插件改配置）；
  静态资源不校验令牌（页面子资源带不上查询串）。
- 响应头带 `Referrer-Policy: no-referrer` 与严格 CSP，兜住「零外部请求」这条约定。
- 进程退出 = 界面关闭；没有常驻服务、不写开机自启。

### 7.2 界面能做什么 / 与 CLI 的对应关系

| 界面页 | 做什么 | 对应 CLI |
| --- | --- | --- |
| 检测 | 列出本机命中的 Agent 与逐条证据（hit/miss） | `detect` |
| 连接网关 | 设备码登录，显示掩码后的凭据状态 | `login` |
| 模型 | 列出网关模型并选一个 | `models` |
| 应用 | 选规格/模型 → **先看差异预览**（`+` 墨绿 / `-` 暗红，已掩码）→ 确认后写入 | `apply --dry-run` → `apply --yes` |
| 体检 | 逐项 PASS / WARN / FAIL | `doctor` |
| 适配器 | 规格全文（也说明怎么新增一个 Agent） | `adapters list` / `adapters show <id>` |

安全口径与 CLI 完全一致：

- 后端返回给前端的凭据**一律掩码**，前端再掩一次（双保险）；页面里不落任何明文密钥。
- **界面不提供查看明文密钥的开关**——要看真值请用 CLI。
- 界面**也没有** `--allow-short-lived-token` 的等价开关：登录得到的临时访问令牌（网关侧约 15 分钟）
  **会被拒绝写入 Agent 配置**，请在「应用」页粘贴 `ximo_sk_` 开头的长期 API Key，或改用 CLI。

### 7.3 截图

本仓库当前**不含界面截图**（`docs/` 与 `internal/ui/assets/` 下都没有图片文件）；
界面外观按 UI 契约 §4 实现（玻璃面板 `backdrop-filter` + 羊皮纸噪点底 + 墨色文字 + 蜡封式状态徽章，
系统字体族、`prefers-reduced-motion` 关闭动效）。

### 7.4 实现状态（照实写）

✅ **实跑**（真二进制，未接真网关）：`ximo-plugin ui --no-open --port 0 --home <临时目录>`
能起界面并打印带令牌地址；`GET /` 返回 200 且页面含羊皮卷/玻璃样式标记；
`GET /api/state` 不带 `X-UI-Token` 返回 **401**（`{"ok":false,"error":{"code":"unauthorized",…}}`），
带正确令牌返回 **200**（`{"ok":true,"data":{…"adapters":[9 条检测证据]}}`）；Ctrl+C / 杀进程后端口即关闭。
`--port 0` 会让系统分配端口，启动打印的是**真实端口**。

尚未验证（📖）：**接了真网关**的完整链路（登录 → 模型 → 计划 → 应用 → 体检）在界面上的表现，
以及界面截图（本仓库暂无）。另有一处已知小瑕疵：`--home` 生效时的「隔离模式」提示行会打印
`%!A(MISSING)PPDATA%`（`cmd/ximo-plugin/ui.go` 把含 `%APPDATA%` 的文案当格式化串用了），
不影响功能，属其他代理的文件，本文档作者未改动。

---

## 8. 相关文档

- 适配器规格总说明：[`../specs/README.md`](../specs/README.md)（含内置 JSON 到底放在哪的说明）
- 规格类型定义：[`../internal/spec/spec.go`](../internal/spec/spec.go)
- 内置规格正文：[`../internal/spec/specs/`](../internal/spec/specs/)
