# 适配器规格库（Specs）

一句话：**加一个新 Agent = 复制一份 JSON 改路径，不用改代码。**

## 1. 文件到底放在哪

| 位置 | 作用 |
| --- | --- |
| `plugin/internal/spec/specs/*.json` | **内置规格正文**（随二进制编译进去，只读） |
| `~/.ximo-plugin/specs/*.json` | 用户自定义规格（可覆盖内置同名 ID；`--home` 可覆盖主目录） |

`plugin/specs/` 这个目录本身只放本说明：`//go:embed` 不能跨目录引用，内置 JSON
必须落在 `internal/spec/` 下才能被编译进二进制，所以正文在
[`internal/spec/specs/`](../internal/spec/specs/)。对外接口
（`spec.Builtin()` / `spec.Load(home)`）与物理位置无关，调用方不用关心这个差异。

内置清单：`aider`、`claude-code`、`codex-cli`、`continue`、`cursor`、
`env-anthropic`、`env-openai`、`openai-compatible-generic`、`ximo-agent`。

## 2. 字段

类型定义（冻结契约 §2）在 [`internal/spec/spec.go`](../internal/spec/spec.go)。

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `id` | ✅ | 小写 slug；**必须与文件名相同**（`my-cli.json` → `"id": "my-cli"`） |
| `name` | ✅ | 展示名 |
| `description` | ✅ | schema 不确定就必须在这里写明“路径/schema 需用户确认” |
| `docs` | | 参考链接，必须是 http(s) |
| `detect` | ✅ | 三选一以上证据：`binaries`（PATH 可执行名）/ `paths`（`~`、`%VAR%`）/ `env`（环境变量名）。**语义是 any-of**：任一命中即算检测到 |
| `env` | | `base_url`/`api_key`/`model` 填**变量名**（不是值！）；`extra` 只作提示、不会被自动赋值；`style` ∈ `export`(默认) / `set` / `powershell` |
| `files` | | 要改的配置文件；`format` ∈ `json`/`toml`/`yaml`/`dotenv`；`create` 为 false 时文件不存在就报错而不是新建 |
| `restart_note` | ✅ | 改完要不要重启进程，写清楚 |
| `protocols` | | `openai-chat` / `anthropic-messages` |

`files[].fields[].path` 是点号路径，数字段表示数组下标，例 `models.0.api_base`；
`from` 只能是 `base_url`/`api_key`/`model`/`display_name`/`gateway_url`/`literal`，
其中 `literal` 必须配 `value`，其它取值不允许带 `value`。

## 3. 新增一个 Agent：三步

1. 挑一份最像的内置规格复制出来。环境变量形态就抄 `claude-code`，
   要改配置文件就抄 `openai-compatible-generic` 或 `ximo-agent`。
2. 存成 `~/.ximo-plugin/specs/<你的 id>.json`，改 `id`/`name`/`detect`/`files[].path`。
   文件名必须与 `id` 相同，否则加载直接报错（防手滑）。
3. 验一遍：

   ```bash
   ximo-plugin adapters list            # 合并后的清单里应能看到你的 id
   ximo-plugin adapters show <你的 id>   # 逐字段回显
   ximo-plugin detect                   # 看你声明的证据到底命不命中
   ximo-plugin apply --spec <你的 id> --dry-run   # 先看差异，再决定写不写
   ```

同 ID 会**整体替换**内置规格（不做字段级合并）；写错字段名、路径不是绝对路径、
含 `..`、`format`/`from` 不在白名单、缺 `restart_note`、`env.api_key` 里塞了 key
字面量——这些都会在校验期直接报错，并一次列出全部问题。

## 4. 最小示例（环境变量形态）

`~/.ximo-plugin/specs/my-cli.json`：

```json
{
  "id": "my-cli",
  "name": "My CLI",
  "description": "示例：换成真实描述；路径/schema 需用户确认。",
  "detect": { "binaries": ["my-cli"], "paths": ["~/.my-cli"] },
  "env": {
    "base_url": "OPENAI_BASE_URL",
    "api_key": "OPENAI_API_KEY",
    "style": "export"
  },
  "restart_note": "需重启终端与 my-cli 进程。",
  "protocols": ["openai-chat"]
}
```

## 5. 最小示例（配置文件形态）

```json
{
  "id": "my-cli-file",
  "name": "My CLI（配置文件）",
  "description": "示例：目标文件为 ~/.my-cli/config.json，schema 需用户确认。",
  "detect": { "paths": ["~/.my-cli/config.json"] },
  "files": [
    {
      "path": "~/.my-cli/config.json",
      "format": "json",
      "create": false,
      "fields": [
        { "path": "providers.0.base_url", "from": "base_url" },
        { "path": "providers.0.api_key", "from": "api_key" },
        { "path": "model", "from": "model" }
      ]
    }
  ],
  "restart_note": "改完重开 my-cli。",
  "protocols": ["openai-chat"]
}
```

## 6. 两条红线

- **不猜第三方 schema**：不确定就把字段省掉、在 `description` 里写“需用户确认”，
  不要凭印象填一堆字段名——写错的字段会破坏别人的配置文件。
- **不明文写 key**：`env.api_key` 只能填变量名；写进文件的凭据必须走
  `internal/gateway` 的 0600 凭据存储或 `secret_ref` 引用，除非用户明确要求落盘。
