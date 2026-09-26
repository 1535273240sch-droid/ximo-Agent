//go:build windows

package main

import (
	"context"

	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
	"github.com/ximo888ok-netizen/ximo-agent/internal/secrets"
)

// secretsForGateway 打开网关用的密钥后端（Windows）。
//
// 顺序与 internal/bootstrap 一致：Windows 凭据管理器（DPAPI 文件后端作自动降级）
// → 环境变量后端。两者都不可用时返回 nil：调用方会得到一个"任何解析都明确报错"的
// 解析器，以及 nil 的密钥写入面 —— admin 侧"带明文密钥的 provider 写入"会明确失败，
// 绝不降级为明文落库（契约 §8 与 admin 包的设计意图）。
//
// dataDir 在 Windows 上不使用（凭据管理器/DPAPI 自带位置），保留参数是为了让两个
// 平台实现具有相同签名。
func secretsForGateway(ctx context.Context, logger *observability.Logger, dataDir string) secretStore {
	_ = dataDir
	mgr, err := secrets.NewDefaultManager()
	if err == nil && mgr.Available() {
		logger.Info(ctx, "ximo-gateway: 密钥后端就绪", map[string]any{"backend": mgr.BackendName()})
		return managerStore{mgr: mgr}
	}
	envMgr, envErr := secrets.NewEnvManager()
	if envErr == nil {
		// 环境变量后端只能读、不能写（Put 要求环境变量预先存在），因此它是"能跑但
		// 不能录入新密钥"的降级态，必须显式 Warning 而不是静默。
		logger.Warn(ctx, "ximo-gateway: 平台安全存储不可用，降级到环境变量后端（只能读取预置密钥，不能写入新密钥）",
			map[string]any{"backend": envMgr.BackendName(), "err": errString(err)})
		return managerStore{mgr: envMgr}
	}
	logger.Warn(ctx, "ximo-gateway: 无可用密钥后端，上游密钥解析与录入都将明确失败",
		map[string]any{"default_err": errString(err), "env_err": envErr.Error()})
	return nil
}

// managerStore 把 *secrets.Manager 适配成 secretStore。
type managerStore struct{ mgr *secrets.Manager }

func (m managerStore) Get(ref string) (string, error)   { return m.mgr.Get(ref) }
func (m managerStore) Put(value string) (string, error) { return m.mgr.Put(value) }
func (m managerStore) BackendName() string              { return m.mgr.BackendName() }
func (m managerStore) Available() bool                  { return m.mgr.Available() }
