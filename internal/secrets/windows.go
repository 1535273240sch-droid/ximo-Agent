//go:build windows

package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// ---------------------------------------------------------------------------
// Windows Credential Manager（advapi32 CredWriteW/CredReadW/CredDeleteW）
// ---------------------------------------------------------------------------

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
	credErrorNotFound       = 1168 // ERROR_NOT_FOUND
)

var (
	modAdvapi32     = syscall.NewLazyDLL("advapi32.dll")
	procCredWriteW  = modAdvapi32.NewProc("CredWriteW")
	procCredReadW   = modAdvapi32.NewProc("CredReadW")
	procCredDeleteW = modAdvapi32.NewProc("CredDeleteW")
	procCredFree    = modAdvapi32.NewProc("CredFree")
)

// credentialW 对应 Windows CREDENTIALW。
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

const credentialTargetPrefix = "ximo-agent/secret/"

func utf16Ptr(s string) (*uint16, error) {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// CredentialBackend 使用 Windows Credential Manager 存储秘密。
type CredentialBackend struct{}

// Name 实现 Backend。
func (b *CredentialBackend) Name() string { return "windows-credential-manager" }

// Available 实现 Backend。
func (b *CredentialBackend) Available() bool {
	return true
}

// Get 实现 Backend。
func (b *CredentialBackend) Get(ref string) (string, error) {
	target, err := utf16Ptr(credentialTargetPrefix + ref)
	if err != nil {
		return "", err
	}
	var cred *credentialW
	ret, _, err := procCredReadW.Call(
		uintptr(unsafe.Pointer(target)),
		uintptr(credTypeGeneric),
		0,
		uintptr(unsafe.Pointer(&cred)),
	)
	if ret == 0 {
		if errno, ok := err.(syscall.Errno); ok && errno == credErrorNotFound {
			return "", fmt.Errorf("%w: %s", ErrSecretNotFound, ref)
		}
		return "", fmt.Errorf("CredReadW 失败: %w", err)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(cred)))
	if cred.CredentialBlobSize == 0 || cred.CredentialBlob == nil {
		return "", fmt.Errorf("%w: %s（空凭据）", ErrSecretNotFound, ref)
	}
	blob := unsafe.Slice(cred.CredentialBlob, cred.CredentialBlobSize)
	// CredentialBlob 以 UTF-16LE 存储。
	u16 := make([]uint16, 0, len(blob)/2)
	for i := 0; i+1 < len(blob); i += 2 {
		u16 = append(u16, uint16(blob[i])|uint16(blob[i+1])<<8)
	}
	return strings.TrimRight(syscall.UTF16ToString(u16), "\x00"), nil
}

// Put 实现 Backend。
func (b *CredentialBackend) Put(ref, value string) error {
	target, err := utf16Ptr(credentialTargetPrefix + ref)
	if err != nil {
		return err
	}
	userName, err := utf16Ptr("ximo-agent")
	if err != nil {
		return err
	}
	// 明文值以 UTF-16LE 写入 CredentialBlob（Credential Manager 自身加密存储）。
	u16, err := syscall.UTF16FromString(value)
	if err != nil {
		return err
	}
	blobSize := uint32(len(u16) * 2)
	cred := credentialW{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: blobSize,
		CredentialBlob:     (*byte)(unsafe.Pointer(&u16[0])),
		Persist:            credPersistLocalMachine,
		UserName:           userName,
		Comment:            nil,
	}
	ret, _, err := procCredWriteW.Call(uintptr(unsafe.Pointer(&cred)), 0)
	if ret == 0 {
		return fmt.Errorf("CredWriteW 失败: %w", err)
	}
	return nil
}

// Delete 实现 Backend。
func (b *CredentialBackend) Delete(ref string) error {
	target, err := utf16Ptr(credentialTargetPrefix + ref)
	if err != nil {
		return err
	}
	ret, _, callErr := procCredDeleteW.Call(
		uintptr(unsafe.Pointer(target)),
		uintptr(credTypeGeneric),
		0,
	)
	if ret == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno == credErrorNotFound {
			return nil // 幂等删除
		}
		return fmt.Errorf("CredDeleteW 失败: %w", callErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// DPAPI 文件后端（CryptProtectData/CryptUnprotectData，与用户账户绑定）
// ---------------------------------------------------------------------------

const (
	cryptprotectUIForbidden = 0x1
	dataBlobSize            = 512 << 10 // 单个秘密文件上限 512KB
)

var (
	modCrypt32             = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = modCrypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = modCrypt32.NewProc("CryptUnprotectData")
)

// dataBlob 对应 Windows DATA_BLOB。
type dataBlob struct {
	cbData uint32
	pbData *byte
}

// DPAPIBackend 用 DPAPI 加密的文件存储秘密（按用户账户绑定）。
// 文件布局：<dir>/<ref 哈希>.dpapi；文件权限 0600。
type DPAPIBackend struct {
	dir string
	mu  sync.Mutex
}

// NewDPAPIBackend 创建 DPAPI 文件后端。
func NewDPAPIBackend(dir string) (*DPAPIBackend, error) {
	if dir == "" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return nil, fmt.Errorf("APPDATA 未设置，无法确定 DPAPI 存储目录")
		}
		dir = filepath.Join(appData, "ximo-agent", "secrets")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建秘密目录失败: %w", err)
	}
	return &DPAPIBackend{dir: dir}, nil
}

// Name 实现 Backend。
func (b *DPAPIBackend) Name() string { return "windows-dpapi" }

// Available 实现 Backend。
func (b *DPAPIBackend) Available() bool {
	// DPAPI 在 Windows 上始终可用（crypt32.dll 属于系统组件）。
	_, err := os.Stat(b.dir)
	return err == nil
}

func (b *DPAPIBackend) path(ref string) string {
	sum := sha256Sum(ref)
	return filepath.Join(b.dir, sum+".dpapi")
}

// Get 实现 Backend。
func (b *DPAPIBackend) Get(ref string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, err := os.ReadFile(b.path(ref))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrSecretNotFound, ref)
		}
		return "", err
	}
	plaintext, err := cryptUnprotectData(data)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// Put 实现 Backend。
func (b *DPAPIBackend) Put(ref, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	encrypted, err := cryptProtectData([]byte(value))
	if err != nil {
		return err
	}
	tmp := b.path(ref) + ".tmp"
	if err := os.WriteFile(tmp, encrypted, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.path(ref)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Delete 实现 Backend。
func (b *DPAPIBackend) Delete(ref string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.Remove(b.path(ref)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func cryptProtectData(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, errors.New("secrets: DPAPI 拒绝加密空数据")
	}
	in := dataBlob{cbData: uint32(len(plaintext)), pbData: &plaintext[0]}
	var out dataBlob
	ret, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		uintptr(cryptprotectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptProtectData 失败: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return copyBlob(out), nil
}

func cryptUnprotectData(encrypted []byte) ([]byte, error) {
	if len(encrypted) == 0 {
		return nil, errors.New("secrets: DPAPI 数据为空")
	}
	in := dataBlob{cbData: uint32(len(encrypted)), pbData: &encrypted[0]}
	var out dataBlob
	ret, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		uintptr(cryptprotectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptUnprotectData 失败（可能是其他用户账户加密的）: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return copyBlob(out), nil
}

func copyBlob(blob dataBlob) []byte {
	if blob.cbData == 0 || blob.pbData == nil {
		return nil
	}
	out := make([]byte, blob.cbData)
	copy(out, unsafe.Slice(blob.pbData, blob.cbData))
	return out
}

var procLocalFree = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")

func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// defaultBackend 选择 Windows 默认后端。
//
// 具体选择逻辑在 fallback.go 的 windowsDefaultBackend：首选 Credential Manager，
// 在非交互式会话下写入失败时自动降级到 DPAPI 文件后端。
func defaultBackend() Backend { return windowsDefaultBackend() }
