# ximo-plugin

**把任意中转站接进你本机已经装好的各种 Agent。** 独立、通用、单个静态二进制——不依赖 ximo-Agent 运行时，也不 import 主仓库任何 `internal/**` 包。

`ximo-plugin` 是一个独立 Go module（`module github.com/1535273240sch-droid/ximo-plugin`），只做两件事，且都做到可解释：

1. 把中转站的 **base_url / 凭据 / 模型名**，按每个 Agent 各自认的形态落进本机（写配置文件，或打印成可执行的环境变量语句）；
2. 提供 `detect` / `doctor` 这类自检，让「到底改了什么、是不是生效了」看得见。

它**不代理流量、不改上游配额**，只负责在你本机把配置接对。

> 对任何 OpenAI / Anthropic 兼容的中转站都一视同仁；本产品自带的 `ximo-gateway`（XIMO 中转站，
> 另一个独立二进制）只是最常见的那一个中转站。

---

## 特性

- **9 个内置适配器（纯数据，不改代码即可扩展）**：`claude-code`、`codex-cli`、`continue`、`cursor`、`aider`、`env-openai`、`env-anthropic`、`openai-compatible-generic`、`ximo-agent`。
- **数据驱动扩展**：加一个新 Agent = 往 `~/.ximo-plugin/specs/` 放一份 JSON，**不用改代码、不用重新编译**（见下「如何新增一个 Agent」）。
- **本地界面**：`ximo-plugin ui` 在回环地址起一个离线网页界面（玻璃质感 + 羊皮卷），把检测 / 登录 / 模型 / 差异预览 / 应用 / 体检串成一条可视化流程；静态资源 `//go:embed` 进二进制，运行期零外部请求。
- **安全默认**：
  - 凭据 0600（目录 0700）原子写入，界面与日志只出现掩码（`gwa_****2c9e`）；
  - 改别人的配置**先备份**（`<path>.bak-<unix>`）、**支持 `--dry-run` 打差异**、**保留未知字段**；
  - 内置规格**不猜第三方 schema**：没核实过的字段名一律不写，宁可选环境变量形态（`specs/*.json` 的 `description` 里逐条写明）；
  - UI 的 `/api/*` 全部要求随机会话令牌（`X-UI-Token`），只绑 `127.0.0.1`。
- **零新依赖**：本 module 只用 `gopkg.in/yaml.v3` 与 `github.com/BurntSushi/toml`（要正确读写第三方 YAML/TOML，手写解析会弄坏别人的文件）。

---

## 安装与构建

需要 **Go 1.27+**。

```bash
git clone https://github.com/1535273240sch-droid/ximo-plugin.git
cd ximo-plugin

go build ./...                                   # 编译整模块（自检）
go vet ./... && go test ./... -count=1            # 本模块很小，可以全量跑
go build -o ximo-plugin ./cmd/ximo-plugin         # Windows 下产物是 ximo-plugin.exe
```

装到 PATH（Linux / macOS）：

```bash
install -m 0755 ximo-plugin ~/.local/bin/ximo-plugin
```

### 交叉编译

```bash
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/ximo-plugin-linux-amd64          ./cmd/ximo-plugin
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/ximo-plugin-darwin-arm64         ./cmd/ximo-plugin
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/ximo-plugin-windows-amd64.exe    ./cmd/ximo-plugin
```

`CGO_ENABLED=0` 得到静态链接产物，拷到同架构的机器上就能跑。

### 发布（Release 附件命名）

推送形如 `v0.1.0` 的 tag 会触发 [`.github/workflows/release.yml`](.github/workflows/release.yml)：矩阵交叉编译 6 个平台，产物命名固定为

```
ximo-plugin_<版本>_<os>_<arch>.zip        # windows
ximo-plugin_<版本>_<os>_<arch>.tar.gz     # linux / darwin
```

例：`ximo-plugin_v0.1.0_windows_amd64.zip`、`ximo-plugin_v0.1.0_linux_arm64.tar.gz`。
版本号由 `-ldflags "-X main.version=<tag>"` 注入（`<tag>` 含 `v` 前缀）。

本地等价做法：

```bash
go build -trimpath -ldflags "-s -w -X main.version=v0.1.0" -o dist/ximo-plugin ./cmd/ximo-plugin
```

发布一个版本：

```bash
git tag v0.1.0 && git push origin v0.1.0     # 推 tag → workflow 自检、编译 6 平台、发 Release
```

> ✅ 版本号注入已实测生效：`main.version` 是可被链接器覆盖的变量，
> `go build -ldflags "-X main.version=v9.9.9-test"` 后 `--version` 打印 `ximo-plugin v9.9.9-test`。

---

## 快速开始

用 `<GW>` 代表你的中转站地址，例 `http://127.0.0.1:8600`。完整链路：
`detect → login → models → apply --dry-run → apply --yes → doctor`。

### CLI

```bash
ximo-plugin detect                                     # 本机检测到哪些 Agent（含证据路径）
ximo-plugin login    --gateway <GW>                    # 设备码登录，凭据落 0600 存储
ximo-plugin models   --gateway <GW>                    # 网关有哪些模型
ximo-plugin apply    --gateway <GW> --all --dry-run     # 先看差异，不落盘
ximo-plugin apply    --gateway <GW> --all --yes         # 确认后写入（自动备份）
ximo-plugin doctor   --gateway <GW>                    # 逐项体检，有问题退出码非 0
ximo-plugin print-env --gateway <GW> --spec claude-code # 只要环境变量语句就够的话
```

全局 flag：`--home <dir>`（覆盖用户主目录，便于隔离测试）、`--json`、`--no-color`。
退出码：`0` 成功 / `1` 运行失败 / `2` 用法错误。
`apply` 默认交互确认，`--yes` 跳过；`--dry-run` 只打印差异。

### 本地界面（UI）

```bash
ximo-plugin ui                 # 默认 http://127.0.0.1:8787，自动打开浏览器
ximo-plugin ui --no-open       # 只打印带令牌的 URL，不自动开浏览器
ximo-plugin ui --port 8899     # 换端口（--port 0 = 让系统分配空闲端口）
ximo-plugin ui --gateway <GW>  # 预填网关；省略时用已保存凭据里的网关
```

打开的地址形如 `http://127.0.0.1:8787/?t=<一次性令牌>`；界面里能依次完成
**检测 / 连接网关（设备码登录）/ 选模型 / 看差异预览 / 应用 / 体检 / 看适配器规格**，
与上面 CLI 命令一一对应。界面**不提供**查看明文密钥的开关（要看真值请用 CLI），
也**没有** `--allow-short-lived-token` 的等价开关——登录得到的临时令牌（约 15 分钟）会被拒绝写入配置，
请在界面里粘贴 `ximo_sk_` 开头的长期 API Key。
细节（含 `--port 0`、`--gateway`、安全边界）见 [`docs/README.md` 的「本地界面（UI）」章节](docs/README.md)。

> ✅ 实测（真二进制、真起服务）：`ximo-plugin ui --no-open --port 0 --home <临时目录>` 起界面并打印带令牌地址；
> `GET /` 200；`/api/state` 不带令牌 **401**、带正确令牌 **200**；杀进程后端口立即关闭。
> 接了真网关的完整链路（登录 / 计划 / 应用 / 体检）尚待在界面上跑通。

---

## 如何新增一个 Agent

**加一个新 Agent = 复制一份 JSON 改路径，不用改代码，也不用重新编译。**

1. 从最像的内置规格复制一份：只认环境变量就抄 `claude-code`；要改配置文件就抄
   `openai-compatible-generic` 或 `ximo-agent`（正文在
   [`internal/spec/specs/`](internal/spec/specs/)，也可用 `ximo-plugin adapters show <id>` 看）。
2. 存成 `~/.ximo-plugin/specs/<你的 id>.json`，改 `id` / `name` / `detect` / `files[].path` 与字段映射。
   **文件名必须与 `id` 完全一致**（`my-cli.json` ↔ `"id": "my-cli"`），否则加载直接报错（防手滑）。
3. 验一遍：

```bash
ximo-plugin adapters list                              # 合并后的清单里应能看到你的 id
ximo-plugin adapters show <你的 id>                     # 逐字段回显
ximo-plugin detect                                     # 你声明的证据到底命不命中
ximo-plugin apply --gateway <GW> --spec <你的 id> --dry-run   # 先看差异再决定写不写
```

最小示例（环境变量形态，存成 `~/.ximo-plugin/specs/my-cli.json`）：

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

要点（写错会在校验期一次列全报错，不会静默写坏文件）：

- `detect` 是 **any-of**：`binaries`（PATH 可执行名）/ `paths`（`~`、`%VAR%`、`$VAR` 展开后存在）/ `env`（已设置的环境变量），任一命中即算检测到；
- `env.base_url` / `env.api_key` 填**变量名**，不是值——往里塞 key 字面量会被拒绝；
- `files[].format` ∈ `json`/`toml`/`yaml`/`dotenv`；`files[].fields[].path` 是点号路径，数字段表示数组下标（例 `providers.0.base_url`）；
- `files[].fields[].from` ∈ `base_url`/`api_key`/`model`/`display_name`/`gateway_url`/`literal`，其中 `literal` 必须配 `value`；
- 同名 ID **整体替换**内置规格（不做字段级合并）。

字段全表与更多示例见 [`specs/README.md`](specs/README.md)。

---

## 安全边界

**设计上就是这么做的：**

- 凭据落 `<home>/.ximo-plugin/cred.json`，以 **0600**（目录 0700）原子写入（临时文件 → chmod → rename），终端/日志/界面只出现掩码。
- 改配置前**自动备份** `<path>.bak-<unix>`；`--dry-run` 不落盘；写失败回滚；只改规格声明的路径，**其它键原样保留**。
- 差异输出**两侧都掩码**；默认**拒绝**写入寿命很短的 access token，必须显式 `--api-key <长期 key>`（`--allow-short-lived-token` 才放行并告警）。
- UI 只绑回环、`/api/*` 必须带会话令牌；后端返回给前端的凭据是掩码后的。

**你需要自己守的边界：**

- **只接入你有授权的服务**。本工具只负责把地址与凭据写进本机 Agent 配置，它**不绕过**上游的认证、限流、计费，也不改变任何服务条款。
- 不要把凭据文件放进共享目录或云同步盘。
- 环境变量形态**不会被自动持久化**：你得自己 `export` / 重开终端（这是刻意的，插件不替你改 shell 配置）。

---

## 已知限制（照实写）

- **第三方 Agent 的字段名未经逐家真实安装验证。** 唯一对着真实代码核对过字段名的文件形态是 `ximo-agent` 的 `provider.{id,name,base_url,model}`（对照 v2 的 `internal/config/config.go` 的 `ProviderConfig`；同结构里的 `secret_ref` **刻意不映射**，明文 key 不进该文件）。除此之外，其余内置适配器**当前都只走环境变量、不改任何第三方文件**——`~/.claude/settings.json`、`~/.codex/config.toml`、`~/.continue/config.yaml`、Cursor 内部存储、`~/.aider.conf.yml` 的 schema 都没核实，猜错字段会破坏别人的配置文件。每个规格的 `description` 里都写明了「路径/schema 需用户确认」，**以 `specs/*.json` 的 `description` 为准**。
- **Windows 无法用权限位验证 0600/0700**：`doctor` 会据此报 WARN，真实保护来自用户目录 ACL。
- **`ximo-agent` 的 IPC 成功路径**需要正在运行的 ximo-agent（Windows 命名管道 `\\.\pipe\ximo-agent-ipc`）；连不上会**回退到改配置文件**。非 Windows 平台默认端点为空，要走 IPC 得显式 `--ipc-endpoint`。
- **`~/.ximo-plugin/openai-compatible.env` 不是任何第三方客户端的官方路径**，它是本插件自己维护的 dotenv 落盘文件，里面是**明文 key**（0600）；不想落盘就只跑 `print-env`。
- **改完基本都要重启**：环境变量只对之后启动的进程生效。各 Agent 的重启口径写在各自规格的 `restart_note` 里（例如 Cursor 必须**完全退出再启动**，Reload Window 不够）。
- **不做**：插件市场 / 清单安装 / 多 provider 轮换 / 自动热重载 Agent。V1 就是「写配置 + 打印环境变量」。
- **`ximo-agent` 规格的默认路径是 Windows 的 `%APPDATA%/ximo-agent/config.json`**；若本机设了 `XIMO_HOME` 或在 macOS/Linux，apply 前必须把 `files[0].path` 改成真实路径。

---

## 文档

| 文档 | 内容 |
| --- | --- |
| [`docs/README.md`](docs/README.md) | 完整手册：逐命令实跑输出、安全与边界、已知限制、本地界面章节 |
| [`docs/QUICKSTART.md`](docs/QUICKSTART.md) | 一页速查（构建 / 跑 / 接一个 Agent / 安全） |
| [`specs/README.md`](specs/README.md) | 适配器规格字段全表与「新增一个 Agent」的三步 |

## 许可

MIT，见 [`LICENSE`](LICENSE)。
