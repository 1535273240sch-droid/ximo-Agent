//go:build darwin

package secrets

/*
// macOS Keychain 后端：通过 cgo 调用 Security framework。
// 秘密以“generic password”形式存放，service=ximo-agent，account=secret_ref。
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <Security/Security.h>
#include <CoreFoundation/CoreFoundation.h>
#include <string.h>
#include <stdlib.h>

// ximo_cfstring 创建 CFString（调用方负责 CFRelease）。
static CFStringRef ximo_cfstring(const char *s) {
    return CFStringCreateWithCString(NULL, s, kCFStringEncodingUTF8);
}

// ximo_keychain_put 写入 generic password。返回 0 成功，非 0 为 OSStatus。
static int ximo_keychain_put(const char *service, const char *account, const char *value) {
    CFStringRef svc = ximo_cfstring(service);
    CFStringRef acct = ximo_cfstring(account);
    CFDataRef data = CFDataCreate(NULL, (const UInt8 *)value, strlen(value));
    CFTypeRef keys[] = {
        kSecClass, kSecAttrService, kSecAttrAccount, kSecValueData
    };
    CFTypeRef vals[] = {
        kSecClassGenericPassword, svc, acct, data
    };
    CFDictionaryRef query = CFDictionaryCreate(NULL,
        (const void **)keys, (const void **)vals, 4,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    if (query == NULL) {
        CFRelease(svc); CFRelease(acct); CFRelease(data);
        return -1;
    }

    OSStatus status = SecItemAdd(query, NULL);
    if (status == errSecDuplicateItem) {
        // 已存在：更新而不是报错（Put 幂等）。
        CFMutableDictionaryRef search = CFDictionaryCreateMutable(NULL, 2,
            &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
        CFDictionaryAddValue(search, kSecClass, kSecClassGenericPassword);
        CFDictionarySetValue(search, kSecAttrService, svc);
        CFDictionarySetValue(search, kSecAttrAccount, acct);
        CFTypeRef updKeys[] = { kSecValueData };
        CFTypeRef updVals[] = { data };
        CFDictionaryRef attrs = CFDictionaryCreate(NULL,
            (const void **)updKeys, (const void **)updVals, 1,
            &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
        status = SecItemUpdate(search, attrs);
        CFRelease(search);
        CFRelease(attrs);
    }
    CFRelease(query);
    // 注意：svc/acct/data 在 duplicate 分支里仍被引用，必须最后释放。
    CFRelease(svc);
    CFRelease(acct);
    CFRelease(data);
    return (int)status;
}

// ximo_keychain_get 读取 generic password。成功时把值写入 buf 并返回长度；
// 失败返回 -1（含 errSecItemNotFound）。
static int ximo_keychain_get(const char *service, const char *account, char *buf, int bufSize) {
    CFStringRef svc = ximo_cfstring(service);
    CFStringRef acct = ximo_cfstring(account);
    CFTypeRef keys[] = { kSecClass, kSecAttrService, kSecAttrAccount, kSecReturnData };
    CFTypeRef vals[] = { kSecClassGenericPassword, svc, acct, kCFBooleanTrue };
    CFDictionaryRef query = CFDictionaryCreate(NULL,
        (const void **)keys, (const void **)vals, 4,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFRelease(svc);
    CFRelease(acct);
    if (query == NULL) return -1;

    CFTypeRef result = NULL;
    OSStatus status = SecItemCopyMatching(query, &result);
    CFRelease(query);
    if (status != errSecSuccess || result == NULL) {
        if (result != NULL) CFRelease(result);
        return -1;
    }
    CFIndex len = CFDataGetLength((CFDataRef)result);
    if (len >= bufSize) len = bufSize - 1;
    memcpy(buf, CFDataGetBytePtr((CFDataRef)result), len);
    buf[len] = '\0';
    CFRelease(result);
    return (int)len;
}

// ximo_keychain_delete 删除 generic password。返回 0 成功（含不存在）。
static int ximo_keychain_delete(const char *service, const char *account) {
    CFStringRef svc = ximo_cfstring(service);
    CFStringRef acct = ximo_cfstring(account);
    CFTypeRef keys[] = { kSecClass, kSecAttrService, kSecAttrAccount };
    CFTypeRef vals[] = { kSecClassGenericPassword, svc, acct };
    CFDictionaryRef query = CFDictionaryCreate(NULL,
        (const void **)keys, (const void **)vals, 3,
        &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    CFRelease(svc);
    CFRelease(acct);
    if (query == NULL) return -1;
    OSStatus status = SecItemDelete(query);
    CFRelease(query);
    if (status == errSecItemNotFound) return 0;
    return (int)status;
}
*/
import "C"

import (
	"fmt"
	"unsafe"
)

const (
	keychainService   = "ximo-agent"
	keychainValueSize = 64 << 10
)

// KeychainBackend 使用 macOS Keychain 存储秘密。
type KeychainBackend struct{}

// Name 实现 Backend。
func (b *KeychainBackend) Name() string { return "macos-keychain" }

// Available 实现 Backend。
func (b *KeychainBackend) Available() bool { return true }

// Get 实现 Backend。
func (b *KeychainBackend) Get(ref string) (string, error) {
	buf := make([]byte, keychainValueSize)
	cService := C.CString(keychainService)
	cRef := C.CString(ref)
	defer C.free(unsafe.Pointer(cService))
	defer C.free(unsafe.Pointer(cRef))
	n := C.ximo_keychain_get(
		cService,
		cRef,
		(*C.char)(unsafe.Pointer(&buf[0])),
		C.int(len(buf)),
	)
	if n < 0 {
		return "", fmt.Errorf("%w: %s", ErrSecretNotFound, ref)
	}
	return string(buf[:n]), nil
}

// Put 实现 Backend。
func (b *KeychainBackend) Put(ref, value string) error {
	cService := C.CString(keychainService)
	cRef := C.CString(ref)
	cValue := C.CString(value)
	defer C.free(unsafe.Pointer(cService))
	defer C.free(unsafe.Pointer(cRef))
	defer C.free(unsafe.Pointer(cValue))
	status := C.ximo_keychain_put(cService, cRef, cValue)
	if status != 0 {
		return fmt.Errorf("SecItemAdd/SecItemUpdate 失败（OSStatus %d）", int(status))
	}
	return nil
}

// Delete 实现 Backend。
func (b *KeychainBackend) Delete(ref string) error {
	cService := C.CString(keychainService)
	cRef := C.CString(ref)
	defer C.free(unsafe.Pointer(cService))
	defer C.free(unsafe.Pointer(cRef))
	status := C.ximo_keychain_delete(cService, cRef)
	if status != 0 {
		return fmt.Errorf("SecItemDelete 失败（OSStatus %d）", int(status))
	}
	return nil
}

// defaultBackend 返回 macOS 默认后端。
func defaultBackend() Backend {
	return &KeychainBackend{}
}
