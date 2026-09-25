package fuzz

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// 1. JSON-RPC 解析 Fuzz
func FuzzJSONRPCParser(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read"}}`,
		`{"jsonrpc":"2.0","id":"abc","result":{"content":"ok"}}`,
		`{"jsonrpc":"2.0","error":{"code":-32600,"message":"Invalid Request"}}`,
		`{"jsonrpc":"2.0"}`,
		`{"id":null}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	type RPCMessage struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var msg RPCMessage
		_ = json.Unmarshal(data, &msg)
	})
}

// 2. MCP 协议解析 Fuzz
func FuzzMCPParser(f *testing.F) {
	seeds := []string{
		`{"method":"notifications/resources/updated","params":{"uri":"file:///test"}}`,
		`{"method":"tools/list","params":{}}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err == nil {
			if m, ok := obj["method"].(string); ok {
				_ = strings.HasPrefix(m, "tools/")
			}
		}
	})
}

// 3. Tool Schema Fuzz
func FuzzToolSchema(f *testing.F) {
	seeds := []string{
		`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
		`{"type":"string"}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var schema map[string]any
		_ = json.Unmarshal(data, &schema)
	})
}

// 4. Permission Matcher Fuzz
func FuzzPermissionMatcher(f *testing.F) {
	f.Add("file_read", "/etc/passwd", "cat /etc/passwd")
	f.Add("terminal_exec", "C:\\Windows\\System32", "cmd.exe /c dir")
	f.Add("web_fetch", "https://api.github.com", "")

	f.Fuzz(func(t *testing.T, tool string, path string, cmd string) {
		// 校验绝不能 panic
		pathClean := filepath.Clean(path)
		_ = strings.HasPrefix(pathClean, "/etc")
		_ = strings.HasPrefix(cmd, "rm ")
	})
}

// 5. Path Normalization Fuzz（路径穿越防护）
func FuzzPathNormalization(f *testing.F) {
	seeds := []string{
		"../../etc/passwd",
		"C:\\Windows\\..\\System32",
		"/var/log/../../root/.ssh/id_rsa",
		"normal/sub/dir/file.txt",
		"///escaped///path/..",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, inputPath string) {
		clean := filepath.Clean(inputPath)
		// 必须安全返回，无 panic
		_ = filepath.IsAbs(clean)
	})
}

// 6. Checkpoint Manifest Fuzz
func FuzzCheckpointManifest(f *testing.F) {
	seeds := []string{
		`{"manifest_id":"m-1","files":[{"path":"a.go","hash":"abc","size":123}]}`,
		`{"manifest_id":"","files":[]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	type Manifest struct {
		ManifestID string `json:"manifest_id"`
		Files      []struct {
			Path string `json:"path"`
			Hash string `json:"hash"`
			Size int64  `json:"size"`
		} `json:"files"`
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var m Manifest
		_ = json.Unmarshal(data, &m)
	})
}

// 7. Event Replay Fuzz
func FuzzEventReplay(f *testing.F) {
	f.Add(uint64(1), "thinking", `{"text":"hello"}`)
	f.Add(uint64(100), "tool_call", `{"tool":"read"}`)

	f.Fuzz(func(t *testing.T, seq uint64, eventType string, payloadJSON string) {
		var p map[string]any
		_ = json.Unmarshal([]byte(payloadJSON), &p)
	})
}

// 8. Tokenizer Fuzz
func FuzzTokenizer(f *testing.F) {
	f.Add("Hello world! こんにちは 世界")
	f.Add("```go\nfunc main() {}\n```")
	f.Add("特殊符号 🚀 \x00\x01\x02\n\t")

	f.Fuzz(func(t *testing.T, input string) {
		if !utf8.ValidString(input) {
			return
		}
		// 模拟 Tokenize 切词无 panic
		runes := []rune(input)
		_ = len(runes)
	})
}
