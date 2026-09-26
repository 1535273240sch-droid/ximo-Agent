package main

import (
	"context"
	"fmt"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/upstream"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/secrets"
)

// secretsForGateway 打开网关用的密钥后端。
//
// 顺序与 internal/bootstrap 一致：平台安全存储（Windows 凭据管理器 → DPAPI 降级）
// → 环境变量后端。两者都不可用时返回 nil：调用方会得到一个"任何解析都明确报错"的
// 解析器，以及 nil 的密钥写入面 —— admin 侧"带明文密钥的 provider 写入"会明确失败，
// 绝不降级为明文落库（契约 §8 与 admin 包的设计意图）。
func secretsForGateway(ctx context.Context, logger *observability.Logger) *secrets.Manager {
	mgr, err := secrets.NewDefaultManager()
	if err == nil && mgr.Available() {
		logger.Info(ctx, "ximo-gateway: 密钥后端就绪", map[string]any{"backend": mgr.BackendName()})
		return mgr
	}
	envMgr, envErr := secrets.NewEnvManager()
	if envErr == nil {
		// 环境变量后端只能读、不能写（Put 要求环境变量预先存在），因此它是"能跑但
		// 不能录入新密钥"的降级态，必须显式 Warning 而不是静默。
		logger.Warn(ctx, "ximo-gateway: 平台安全存储不可用，降级到环境变量后端（只能读取预置密钥，不能写入新密钥）",
			map[string]any{"backend": envMgr.BackendName(), "err": errString(err)})
		return envMgr
	}
	logger.Warn(ctx, "ximo-gateway: 无可用密钥后端，上游密钥解析与录入都将明确失败",
		map[string]any{"default_err": errString(err), "env_err": envErr.Error()})
	return nil
}

// errString 把可能为 nil 的错误转成日志字段值。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// secretResolver 把 internal/secrets.Manager 适配成 upstream.SecretResolver。
//
// 两个包的形状不同（secrets 是 Get(ref)，upstream 要 Resolve(ctx, ref)），且
// upstream 的接口按惯例由消费方定义，因此这层适配只能由装配方（本包）提供 ——
// 见契约 §12.1"注入一个把 internal/secrets.Manager 适配成 SecretResolver 的小适配器"。
//
// 解析失败一律返回错误而不是空串：带着空 Authorization 头去请求上游，会得到一个
// 分不清"密钥没配"与"密钥配错"的 401。错误信息里只出现 secret_ref（是明文的单向
// 摘要，不是密钥本身）。
type secretResolver struct{ mgr *secrets.Manager }

var _ upstream.SecretResolver = secretResolver{}

func (r secretResolver) Resolve(_ context.Context, ref string) (string, error) {
	if r.mgr == nil {
		return "", fmt.Errorf("ximo-gateway: 没有可用的平台安全存储，无法解析 %s", ref)
	}
	return r.mgr.Get(ref)
}
