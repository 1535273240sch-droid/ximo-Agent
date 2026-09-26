# XimoAgent v2.2.1 更新说明

发布日期：2026-09-26 ｜ 版本跨度：v2.2.0 → v2.2.1

## 一、本次更新概述

v2.2.0 之后的收尾版本：补上中转站的**管理控制台**（此前浏览器打开网关地址只会看到 404，看起来像"后端进不去"），修掉两处跨平台缺陷，并补上 Linux 服务器上的部署与验证材料。桌面端渲染层无改动。

## 二、新增

### 1. 管理控制台（`/console/`）

- 打开 `http://<网关地址>:8600/` 现在会 **302 到 `/console/`**，不再是 404。
- 页面：概览、**设备码批准**（插件登录的最后一步）、用户与额度（建用户/启停/充值/扣减/冻结/账本）、API 密钥（签发只显示一次/吊销）、模型与 Provider、用量与审计。
- 鉴权边界：**页面本身是静态资源、不需要令牌**；令牌由使用者在页面上输入，只存在浏览器 localStorage，每次数据请求以 `X-Admin-Token` 发给 `/admin/*`，由服务端逐个校验。因此把控制台暴露在公网不会泄露数据；但令牌是明文传输的，生产应放在 HTTPS 之后。
- 沿用手工实现的玻璃质感 + 羊皮卷样式，与插件界面一致。
- 路由细节（都加了测试钉住）：根路径用 `GET /{$}` 精确匹配——用 `GET /` 会变成 catch-all 把未知 API 路径也重定向成 HTML；也不能同时注册 `/console/` 与 `/console/{file...}`（Go 1.22+ ServeMux 判为冲突并拒绝启动）。

### 2. 部署材料（`deploy/`）

systemd 单元、幂等安装脚本、端到端验证脚本与假上游、设备码批准脚本，以及 Linux 服务器部署说明（含管理控制台的用法与安全提醒）。

## 三、修复

1. **插件 `--home` 隔离在非 Windows 上"逃逸"**：隔离变量表此前按平台裁剪（只在 Windows 给 `APPDATA`），导致 Linux/macOS 上 `%APPDATA%/...` 仍解析到真实用户目录。规格 JSON 是跨平台共享的数据，隔离模式下这些变量现在在任何平台都按隔离 home 反推（CI 在 ubuntu-latest 上实测到该失败，已修）。
2. **插件一处隔离用例的 Windows-only 断言**：它断言 `%APPDATA%` 与 `%XIMO_HOME%` 必须指向同一路径——这在 Windows 成立，在 Linux/macOS 天然不同（`agentBaseDirIn` 按 XDG/Library 约定走，属有意行为）。改为断言跨平台成立的不变量：两者都落在隔离 home 之下。

## 四、Linux 可部署性（网关侧）

- 新增 `internal/gateway/secretref`：`secret_ref` 的派生与校验，规则与 `internal/secrets` 逐字一致，但不依赖平台安全存储。
- 网关的密钥后端按平台拆分：Windows 继续用凭据管理器/DPAPI；其他平台用 **0600 文件后端**（启动时显式 WARN 说明明文落盘，不静默降级）。
- 现状：`GOOS=linux go build/vet ./cmd/ximo-gateway/... ./internal/gateway/...` 干净通过，并已在 Ubuntu 26.04 上以 systemd 托管运行、通过 18 项端到端断言。
- **仍待修**：`internal/secrets`、`internal/ipc`、`internal/worker/procguard` 三个包在非 Windows 下无法编译，完整引擎的 Linux 构建仍不成立（README 中相关表述不准确）。

## 五、已知事项

- 中转站不把 `usage` 分片转发给 SSE 客户端：流式响应里没有 `usage` 字段，token 数请查 `GET /v1/usage`（上游 usage 的解析与计费正常）。
- 计价为占位价（`--price-micro-per-ktok`），上线前需按真实成本配置。
- 管理 API 没有删除接口：Provider/模型只能用 upsert 置为 `disabled`。
- 第三方 Agent 的配置文件字段名未经逐家真机验证，对应适配器只设置环境变量、不写它们的配置文件。
