//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ximo888ok-netizen/ximo-agent/internal/gateway/secretref"
	"github.com/ximo888ok-netizen/ximo-agent/internal/observability"
)

// secretsForGateway 打开网关用的密钥后端（Linux/macOS 等非 Windows 平台）。
//
// 为什么是文件后端：内部/平台安全存储没有跨平台可用实现（DPAPI 是 Windows 专有，
// libsecret 需要 dbus 会话），而网关必须能存上游密钥。这里用 dataDir 下的
// secrets.json（0600、原子写）作为后端 —— 它**明文落盘**，因此启动时显式 Warning，
// 并由调用方保证 dataDir 是私有目录（默认与数据库同级，在网关的 home 下）。
//
// 与 Windows 侧的差异：ref 的派生规则完全相同（secretref 包），所以 secret_ref
// 字符串在两侧可互相识别；但密钥**值**必须各自录入一次（存储位置不同）。
// dbPath 是网关数据库的路径；密钥文件放在它的同级目录（即网关自己的 data 目录）。
func secretsForGateway(ctx context.Context, logger *observability.Logger, dbPath string) secretStore {
	path := filepath.Join(filepath.Dir(dbPath), "secrets.json")
	st, err := newFileStore(path)
	if err != nil {
		logger.Warn(ctx, "ximo-gateway: 密钥文件后端不可用，上游密钥解析与录入都将明确失败",
			map[string]any{"path": path, "err": err.Error()})
		return nil
	}
	logger.Info(ctx, "ximo-gateway: 密钥后端就绪", map[string]any{"backend": st.BackendName(), "path": path})
	logger.Warn(ctx, "ximo-gateway: 本平台无系统级密钥库，上游密钥以 0600 文件明文保存；请确保该目录不对其他用户开放",
		map[string]any{"path": path, "mode": "0600"})
	return st
}

// fileStore 是 0600 JSON 文件后端：{ "<secretref:v1:...>": "<明文>" }。
//
// 键用 ref（明文的单向摘要）而不是明文本身，便于日志与审计里只出现 ref。
type fileStore struct {
	path string

	mu     sync.Mutex
	values map[string]string
}

var _ secretStore = (*fileStore)(nil)

func newFileStore(path string) (*fileStore, error) {
	if path == "" {
		return nil, errors.New("secrets: 未指定密钥文件路径")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("secrets: 创建目录失败: %w", err)
	}
	st := &fileStore{path: path, values: map[string]string{}}
	if err := st.load(); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *fileStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动：空表即可，无需建文件
		}
		return fmt.Errorf("secrets: 读取失败: %w", err)
	}
	if len(raw) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("secrets: 解析失败（文件可能损坏，请人工确认）: %w", err)
	}
	if m != nil {
		s.values = m
	}
	return nil
}

// save 原子写：同目录临时文件（先 chmod 0600）→ rename 覆盖。
func (s *fileStore) save() error {
	raw, err := json.Marshal(s.values)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".secrets-*.tmp")
	if err != nil {
		return fmt.Errorf("secrets: 创建临时文件失败: %w", err)
	}
	tmp := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("secrets: 设置权限失败: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("secrets: 写入失败: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("secrets: 关闭文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("secrets: 替换失败: %w", err)
	}
	return nil
}

func (s *fileStore) Get(ref string) (string, error) {
	if !secretref.IsRef(ref) {
		return "", fmt.Errorf("secrets: 不是合法的 secret_ref: %s", ref)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[ref]
	if !ok {
		return "", fmt.Errorf("secrets: 未找到 %s", ref)
	}
	return v, nil
}

func (s *fileStore) Put(value string) (string, error) {
	if value == "" {
		return "", errors.New("secrets: 拒绝写入空密钥")
	}
	ref := secretref.ForValue(value)
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.values[ref]; ok && cur == value {
		return ref, nil // 幂等：同值同 ref，不重复写盘
	}
	s.values[ref] = value
	if err := s.save(); err != nil {
		delete(s.values, ref) // 写盘失败则回滚内存，避免内存与磁盘不一致
		return "", err
	}
	return ref, nil
}

func (s *fileStore) BackendName() string { return "file-0600" }

func (s *fileStore) Available() bool { return s != nil }

// refs 返回已存 ref（按字典序），仅用于日志/诊断，不暴露明文。
func (s *fileStore) refs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.values))
	for k := range s.values {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
