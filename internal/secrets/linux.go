//go:build linux

package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// secretToolTimeout secret-tool 子进程超时（避免 keyring 卡死拖住调用方）。
const secretToolTimeout = 10 * time.Second

// SecretServiceBackend 通过 libsecret 的 secret-tool CLI 访问
// Linux Secret Service（GNOME Keyring / KWallet 兼容层）。
type SecretServiceBackend struct {
	tool string // secret-tool 路径
}

// NewSecretServiceBackend 创建 Secret Service 后端。secret-tool 不可用时
// 返回错误（绝不退化为明文文件存储）。
func NewSecretServiceBackend() (*SecretServiceBackend, error) {
	tool, err := exec.LookPath("secret-tool")
	if err != nil {
		return nil, fmt.Errorf("%w: secret-tool 未安装（libsecret-tools）", ErrBackendUnavailable)
	}
	return &SecretServiceBackend{tool: tool}, nil
}

// Name 实现 Backend。
func (b *SecretServiceBackend) Name() string { return "linux-secret-service" }

// Available 实现 Backend。
func (b *SecretServiceBackend) Available() bool { return b.tool != "" }

// Get 实现 Backend。
func (b *SecretServiceBackend) Get(ref string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), secretToolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.tool, "lookup", "service", "ximo-agent", "account", ref)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "not found") || stdout.Len() == 0 {
			return "", fmt.Errorf("%w: %s", ErrSecretNotFound, ref)
		}
		return "", fmt.Errorf("secret-tool lookup 失败: %v (%s)", err, strings.TrimSpace(stderr.String()))
	}
	value := strings.TrimRight(stdout.String(), "\n")
	if value == "" {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, ref)
	}
	return value, nil
}

// Put 实现 Backend。
func (b *SecretServiceBackend) Put(ref, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), secretToolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.tool, "store", "--label=ximo-agent "+ref,
		"service", "ximo-agent", "account", ref)
	cmd.Stdin = strings.NewReader(value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("secret-tool store 失败: %v (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Delete 实现 Backend。
func (b *SecretServiceBackend) Delete(ref string) error {
	ctx, cancel := context.WithTimeout(context.Background(), secretToolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.tool, "clear", "service", "ximo-agent", "account", ref)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("secret-tool clear 失败: %v (%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// defaultBackend 返回 Linux 默认后端。
func defaultBackend() Backend {
	backend, err := NewSecretServiceBackend()
	if err != nil {
		return &unavailableBackend{name: "linux-secret-service-unavailable", reason: err.Error()}
	}
	return backend
}

// unavailableBackend 在后端不可用时显式失败（绝不静默退化为明文）。
type unavailableBackend struct {
	name   string
	reason string
}

func (b *unavailableBackend) Name() string { return b.name }

func (b *unavailableBackend) Available() bool { return false }

func (b *unavailableBackend) Get(ref string) (string, error) {
	return "", fmt.Errorf("%w: %s", ErrBackendUnavailable, b.reason)
}

func (b *unavailableBackend) Put(ref, value string) error {
	return fmt.Errorf("%w: %s", ErrBackendUnavailable, b.reason)
}

func (b *unavailableBackend) Delete(ref string) error {
	return fmt.Errorf("%w: %s", ErrBackendUnavailable, b.reason)
}
