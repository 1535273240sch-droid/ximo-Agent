# 中转站部署（Linux 服务器）

本目录是 `ximo-gateway` 在生产服务器上的部署与验证材料。当前已在
**`175.178.161.9`（Ubuntu 26.04 / 4 核 7.6G，腾讯云）** 上运行，systemd 托管、开机自启。

## 目录内容

| 文件 | 用途 |
| --- | --- |
| `ximo-gateway.service` | systemd 单元（含启动参数与最小权限设置） |
| `setup-gateway.sh` | 幂等安装/更新脚本：生成管理令牌（首次）→ 装单元 → 重启 → 健康检查 |
| `e2e-gateway.sh` | 端到端验证：建用户→幂等充值→配 Provider→建模型→发 Key→补全（流式/非流式）→用量→账本对账→402/404 |
| `fake-upstream.py` | 验证用的假上游（OpenAI 兼容，usage 分片放在 finish_reason **之后**，模拟真实上游形态） |

## 管理控制台（浏览器可登录的界面）

打开 **<http://175.178.161.9:8600/>** 会被重定向到 `/console/`，那里是管理控制台：

| 页面 | 能做什么 |
| --- | --- |
| 概览 | 服务状态/版本/用户数/模型数/Provider 数 |
| 设备码批准 | 粘插件给出的用户码 → 批准（**这是插件登录的最后一步**） |
| 用户与额度 | 建用户、启用/禁用、充值/扣减/冻结、看账本 |
| API 密钥 | 给用户签发密钥（只显示一次）、吊销 |
| 模型与 Provider | 配上游服务商与密钥、建模型并加映射 |
| 用量与审计 | 按用户查用量；所有管理写操作留痕 |

首次进入把管理令牌（`cat ~/ximo-gateway/gateway.env`）填进右上角输入框并点「连接」。
令牌只存在**本机浏览器**（localStorage），每次请求以 `X-Admin-Token` 发送；
页面本身是静态资源、不需要令牌，因此把它暴露在公网不会泄露数据 —— 但**令牌是明文
传输的**，务必尽快配上 HTTPS（见下面的安全提醒）。

## 首次部署

```bash
# 1) 本地交叉编译（网关不依赖 cgo，也不需要服务器装 Go）
GOROOT=/c/ximo-tools/go PATH=/c/ximo-tools/go/bin:$PATH \
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" \
  -o dist/ximo-gateway-linux-amd64 ./cmd/ximo-gateway

# 2) 上传到服务器（假设目录 /home/ubuntu/ximo-gateway）
#    scp dist/ximo-gateway-linux-amd64 ubuntu@<host>:~/ximo-gateway/ximo-gateway
#    scp migrations/000*.sql            ubuntu@<host>:~/ximo-gateway/migrations/
#    scp deploy/ximo-gateway.service deploy/setup-gateway.sh ubuntu@<host>:~/ximo-gateway/

# 3) 在服务器上执行（幂等，可重复用于升级）
bash ~/ximo-gateway/setup-gateway.sh
```

管理令牌在**服务器本地**用 `openssl rand -hex 32` 生成，写入 `~/ximo-gateway/gateway.env`
（0600），不会出现在聊天记录或进程列表里。查看：

```bash
cat ~/ximo-gateway/gateway.env          # XIMO_GATEWAY_ADMIN_TOKEN=<...>
```

## 验证

```bash
cd ~/ximo-gateway
python3 fake-upstream.py &              # 假上游，仅验证用
bash e2e-gateway.sh                     # 全部 PASS 才是通过（末尾打印汇总）
pkill -f "fake-upstream\.py"            # 收尾
```

## 更新（换二进制）

```bash
# 上传新二进制覆盖 ~/ximo-gateway/ximo-gateway，然后：
bash ~/ximo-gateway/setup-gateway.sh    # 不会重新生成令牌，只重启
```

## 当前部署事实（可核对）

- 监听 `0.0.0.0:8600`，健康检查 `GET /v1/health`；**外网可直连**（腾讯云安全组已放通 8600）。
- 数据库：`~/ximo-gateway/data/gateway.db`（SQLite，含 0001–0003 迁移）。
- 密钥后端：**`file-0600`** —— 本平台没有系统级密钥库（DPAPI 是 Windows 专有，libsecret 需要
  dbus 会话），上游密钥以 0600 文件明文保存在 `~/ximo-gateway/data/secrets.json`；
  启动日志里有一条对应的 WARN，这是有意的显式告警，不是故障。
- 计价：`--price-micro-per-ktok 1000000`（即 1 credit/千 token）是**占位价**，上线前必须按真实成本改。
- 预占回收：`--reap-interval 1m`（结算失败时靠它兜底，避免额度被永久占用）。

## 安全提醒（重要）

1. **当前是明文 HTTP**：管理令牌与用户 API Key 会以明文经过网络。生产建议走 HTTPS——
   你机器上已有 nginx（站点 `digital-human`），可以给它加一个 server 块反代到
   `127.0.0.1:8600`，或申请证书后把网关只绑回环。
2. **管理接口没有 RBAC/MFA**：`X-Admin-Token` 是唯一凭据，务必只放在可信机器上；
   泄露后用 `openssl rand -hex 32` 重新生成并重启。
3. `data/` 目录含数据库与明文密钥文件，确保不对其他用户开放（当前为 0700/0600）。
4. 该机器上还有你原有的服务（nginx `digital-human` → 127.0.0.1:8801 uvicorn、7864 的 Web 应用），
   本次部署**没有改动它们**，只新增了 8600 端口与一个 systemd 单元。

## 已知限制

- 网关**不把 usage 分片转发给 SSE 客户端**：流式响应里没有 `usage` 字段，token 数要查
  `GET /v1/usage`。上游 usage 的解析与计费是正常工作的（E2E 第 9 步用服务端 usage 行验证）。
- 管理 API 没有删除接口：验证残留的假 Provider/模型只能用 upsert 置为 `disabled`
  （当前已全部禁用），测试用户保留以便查看账本。
- 仓库里 `internal/secrets`、`internal/ipc`、`internal/worker/procguard` 三个包目前**只能在
  Windows 上编译**（README 里"Linux/macOS 可编译"的说法在 `v2` 分支上不成立）。
  网关已通过 `internal/gateway/secretref` + 按平台拆分的密钥后端绕开这个限制，
  但完整引擎的 Linux 构建仍需修那三个包。
