package secrets

import (
	"fmt"
	"os"
	"strings"
)

// EnvBackend 从环境变量读取秘密（仅供测试/CI/无头环境显式选择）。
// 环境变量是进程级明文，因此默认后端永远不会选它；只有调用方显式
// NewEnvManager() 才启用，且 Available() 恒为 true。
//
// 环境变量命名：XIMO_SECRET_<ref 的非前缀部分大写>，
// 例如 secretref:v1:abcd... -> XIMO_SECRET_ABCD...
type EnvBackend struct{}

// NewEnvManager 创建基于环境变量的秘密管理器（测试用）。
func NewEnvManager() (*Manager, error) {
	return NewManager(&EnvBackend{})
}

// Name 实现 Backend。
func (b *EnvBackend) Name() string { return "env" }

// Available 实现 Backend。
func (b *EnvBackend) Available() bool { return true }

func envKey(ref string) string {
	body := strings.TrimPrefix(ref, refPrefix)
	return "XIMO_SECRET_" + strings.ToUpper(body)
}

// Get 实现 Backend。
func (b *EnvBackend) Get(ref string) (string, error) {
	value := os.Getenv(envKey(ref))
	if value == "" {
		return "", fmt.Errorf("%w: %s（环境变量 %s 未设置）", ErrSecretNotFound, ref, envKey(ref))
	}
	return value, nil
}

// Put 实现 Backend。环境变量无法跨进程持久化，这里只做存在性校验：
// 真实写入应由运维在进程启动前完成。
func (b *EnvBackend) Put(ref, value string) error {
	key := envKey(ref)
	if os.Getenv(key) == "" {
		return fmt.Errorf("secrets: 环境变量 %s 未预先设置，EnvBackend 不能写入", key)
	}
	return nil
}

// Delete 实现 Backend（环境变量无法删除，保持幂等）。
func (b *EnvBackend) Delete(ref string) error { return nil }

// DefaultBackend 返回当前平台的默认安全后端。
// 由各平台文件（windows.go/macos.go/linux.go）提供具体实现。
func DefaultBackend() Backend {
	return defaultBackend()
}
