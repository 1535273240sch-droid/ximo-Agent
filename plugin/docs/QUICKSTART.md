# ximo-plugin — Quickstart

Connect any OpenAI/Anthropic-compatible gateway to agents already installed on this machine.
Single static binary, standalone Go module, no dependency on the ximo-Agent runtime.
Full docs (Chinese, with verification notes): [README.md](README.md).

## Build

```bash
cd plugin
go build ./... && go vet ./... && go test ./... -count=1

go build -o ximo-plugin.exe ./cmd/ximo-plugin                          # Windows amd64
GOOS=linux   GOARCH=amd64 go build -o ximo-plugin-linux-amd64 ./cmd/ximo-plugin
GOOS=darwin  GOARCH=arm64 go build -o ximo-plugin-darwin-arm64 ./cmd/ximo-plugin
```

Artifacts land in the current directory (`plugin/`).

## Run

```bash
GW=http://127.0.0.1:8600

ximo-plugin detect                              # which agents are present, with evidence
ximo-plugin login    --gateway $GW              # device-code login; credentials -> ~/.ximo-plugin/cred.json (0600)
ximo-plugin models   --gateway $GW              # list models offered by the gateway
ximo-plugin apply    --gateway $GW --spec claude-code --dry-run   # show the diff, write nothing
ximo-plugin apply    --gateway $GW --spec claude-code --yes       # apply (interactive confirm without --yes)
ximo-plugin doctor   --gateway $GW              # PASS/FAIL self-check; non-zero exit on FAIL
ximo-plugin print-env --gateway $GW --spec env-openai --style export
```

Global flags: `--home <dir>`, `--json`, `--no-color`. Exit codes: `0` ok, `1` runtime failure, `2` usage error.

## Adapting another agent

One JSON file per agent — no code changes, no recompile. Copy the closest built-in spec from
`plugin/internal/spec/specs/`, then save yours as `~/.ximo-plugin/specs/<id>.json`
(the filename **must** equal the `id`):

```json
{
  "id": "my-cli",
  "name": "My CLI",
  "description": "Describe it; say \"schema needs confirmation\" when unsure.",
  "detect": { "binaries": ["my-cli"], "paths": ["~/.my-cli"] },
  "env": { "base_url": "OPENAI_BASE_URL", "api_key": "OPENAI_API_KEY", "style": "export" },
  "restart_note": "Restart the terminal and my-cli.",
  "protocols": ["openai-chat"]
}
```

Then verify: `ximo-plugin adapters show my-cli` and
`ximo-plugin apply --gateway $GW --spec my-cli --dry-run`.

A user spec with the same `id` replaces the built-in one entirely. Unknown fields, a filename/id
mismatch, a relative path, or a key literal in `env.api_key` are all rejected at load time.

## Safety

- Credentials are stored 0600 and never logged; output shows only a mask (`gwa_****2c9e`).
- No built-in spec maps `api_key` into a **third-party** agent config.
- Files are backed up to `<path>.bak-<unix>`; `--dry-run` writes nothing; failed writes roll back.
- Unknown fields in existing config files are preserved.
- Only connect to services you are authorized to use. This tool does not bypass upstream
  authentication, rate limits, or billing.

## Known caveats

- `apply --dry-run` masks credentials on **both** sides of the diff (old value and new value);
  `--show-secrets` prints real values, so do not redirect that into logs.
- Writing a short-lived access token into an agent config is **refused by default**; pass
  `--api-key <long-lived key>` or explicitly opt in with `--allow-short-lived-token`.
- Missing parent directories are created automatically (0700).
- Field names for `claude-code`, `codex-cli`, `continue`, `cursor`, `aider` config files are
  **not verified** — those adapters only set environment variables. See each spec's `description`.
- Environment variables only affect processes started afterwards; restart the agent.
- `ximo-agent` IPC is Windows-only and was **not** exercised against a running ximo-agent.
