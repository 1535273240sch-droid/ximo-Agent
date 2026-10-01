//go:build windows

package office

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// Windows 专有测试：8.3 短名与长名是"同一个目录"的两种等价写法。
// 单独放一个文件是为了让上面的通用测试保持跨平台。

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetShortPathNameW = kernel32.NewProc("GetShortPathNameW")
	procGetLongPathNameW  = kernel32.NewProc("GetLongPathNameW")
)

// winPathName 调用 kernel32 的短名/长名转换（不引入任何新依赖）。
func winPathName(t *testing.T, proc *syscall.LazyProc, p string) string {
	t.Helper()
	ptr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		t.Fatalf("路径转 UTF16 失败: %v", err)
	}
	// GetShortPathNameW 在"该卷未生成 8.3 名"时返回长名本身（长度为 0 才算调用失败），
	// 因此这里不把"结果 == 输入"当成错误，交给调用方 skip。
	buf := make([]uint16, syscall.MAX_PATH)
	n, _, callErr := proc.Call(uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		t.Fatalf("%s 调用失败: %v", proc.Name, callErr)
	}
	return syscall.UTF16ToString(buf[:n])
}

// TestResolvePathShortNameEqualsLongName 验证 Windows 8.3 短名与长名指向同一目录时，
// 白名单判定必须一致。
//
// 这正是本机 t.TempDir() 触发假性拒绝的根因：%TEMP% 是 C:\Users\ADMINI~1\... 短名，
// 而 EvalSymlinks 只能把"已存在"的路径还原成长名；目标文件尚不存在（create 场景）时
// 它直接报错，旧实现于是拿"短名原串"去比对"长名 root"，把白名单内的路径判成越权。
func TestResolvePathShortNameEqualsLongName(t *testing.T) {
	root := t.TempDir()
	short := winPathName(t, procGetShortPathNameW, root)
	long := winPathName(t, procGetLongPathNameW, root)
	if strings.EqualFold(short, long) {
		t.Skipf("该卷未生成 8.3 短名，跳过: %s", root)
	}

	// 白名单写长名，调用方用短名给出一个"尚未创建"的文件（create 场景）。
	wLong := NewWorker("office-0", Config{AllowedRoots: []string{long}})
	got, err := wLong.resolvePath(filepath.Join(short, "new.docx"))
	if err != nil {
		t.Errorf("短名路径应被认定在白名单（长名）内: %v", err)
	} else if want := filepath.Join(long, "new.docx"); !strings.EqualFold(got, want) {
		// 返回值必须收敛到长名：把短名透传给 officecli 会让"被校验的路径"和
		// "真正被写入的路径"再次出现两套写法。
		t.Errorf("应返回规范化后的长名路径: got=%s want=%s", got, want)
	}

	// 反向：白名单写短名，调用方写长名。
	wShort := NewWorker("office-0", Config{AllowedRoots: []string{short}})
	if _, err := wShort.resolvePath(filepath.Join(long, "new.docx")); err != nil {
		t.Errorf("长名路径应被认定在白名单（短名）内: %v", err)
	}

	// 兼容短名写法不等于放宽白名单：另一个目录（同样以短名给出）仍必须被拒。
	outside := t.TempDir()
	outsideShort := winPathName(t, procGetShortPathNameW, outside)
	if _, err := wLong.resolvePath(filepath.Join(outsideShort, "x.docx")); err == nil {
		t.Error("白名单外目录（短名写法）必须仍被拒绝")
	}
}
