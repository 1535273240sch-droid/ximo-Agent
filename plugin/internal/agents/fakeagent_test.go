package agents

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
)

// fakeAgent 是一个**进程内的假 ximo-agent IPC 服务端**：按协议帧格式收包，
// 按主仓库 internal/ipcapi 的语义回 {ok,error,data} 信封。
//
// 它刻意复刻 agent 侧 ApplySettings 的合并语义（provider_id/name/base_url/
// model/workspace_root 无条件覆盖，secret_ref/auto_mode 非空才覆盖，
// providers/mcp_servers 为 nil 表示沿用），这样「插件发错了载荷会把用户配置
// 清空」这类问题在测试里就能暴露，而不是等到真实 agent 上才发现。
type fakeAgent struct {
	t        *testing.T
	listener net.Listener
	endpoint string

	mu            sync.Mutex
	settings      RuntimeSettings
	secrets       map[string]string // ref -> 明文（只在测试进程内）
	setPayloads   []RuntimeSettings
	putValues     []string
	getCalls      int
	setCalls      int
	modelLists    []ModelInfo
	secretAvail   bool
	failConfigGet bool
	// leakKeyInError 让服务端故意把收到的明文塞进错误消息，用来验证客户端
	// 侧的脱敏真的生效（这是刻意的「坏服务端」反向控制）。
	leakKeyInError bool
	// preludeFrames 是每次响应前先塞的「无关帧」（模拟服务端广播），
	// 用来验证客户端按 RequestID 配对而不是读到什么就当成什么。
	preludeFrames int

	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
}

func newFakeAgent(t *testing.T, initial RuntimeSettings) *fakeAgent {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动假 IPC 服务端失败: %v", err)
	}
	f := &fakeAgent{
		t:           t,
		listener:    ln,
		endpoint:    "tcp://" + ln.Addr().String(),
		settings:    initial,
		secrets:     map[string]string{},
		secretAvail: true,
		done:        make(chan struct{}),
	}
	f.wg.Add(1)
	go f.serve()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeAgent) stop() {
	f.closeOnce.Do(func() {
		close(f.done)
		_ = f.listener.Close()
	})
	f.wg.Wait()
}

func (f *fakeAgent) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer conn.Close()
			f.handleConn(conn)
		}()
	}
}

func (f *fakeAgent) handleConn(conn net.Conn) {
	for {
		req, err := ReadFrame(conn, DefaultMaxPayload)
		if err != nil {
			return
		}
		f.mu.Lock()
		for i := 0; i < f.preludeFrames; i++ {
			f.mu.Unlock()
			// 一条无关的广播帧：RequestID 为空、类型不在请求序列里。
			_ = WriteFrame(conn, &Frame{
				Header:  FrameHeader{Version: CurrentVersion, Type: "engine.event.stream"},
				Payload: []byte(`{"ok":true,"data":{"run_id":"noise"}}`),
			})
			f.mu.Lock()
		}
		f.mu.Unlock()

		resp := f.dispatch(req)
		if resp == nil {
			continue
		}
		if err := WriteFrame(conn, resp); err != nil {
			return
		}
	}
}

func (f *fakeAgent) dispatch(req *Frame) *Frame {
	switch req.Header.Type {
	case TypeHeartbeat:
		return &Frame{
			Header:  FrameHeader{Version: CurrentVersion, RequestID: req.Header.RequestID, Sequence: req.Header.Sequence + 1, Type: TypeHeartbeatAck},
			Payload: []byte("OK"),
		}
	case TypeConfigGet:
		f.mu.Lock()
		f.getCalls++
		fail := f.failConfigGet
		cur := f.settings
		f.mu.Unlock()
		if fail {
			return errorEnvelopeFrame(req, fmt.Errorf("settings are not available in this build"))
		}
		return okEnvelopeFrame(req, cur)
	case TypeSecretPut:
		var p struct {
			Value string `json:"value"`
		}
		if err := decodeRequestEnvelope(req, &p); err != nil {
			return errorEnvelopeFrame(req, err)
		}
		if p.Value == "" {
			return errorEnvelopeFrame(req, fmt.Errorf("value must not be empty"))
		}
		f.mu.Lock()
		f.putValues = append(f.putValues, p.Value)
		avail := f.secretAvail
		leaky := f.leakKeyInError
		ref := ""
		if !avail {
			f.mu.Unlock()
			if leaky {
				// 故意泄露：验证客户端侧的 redact 是最后一道闸。
				return errorEnvelopeFrame(req, fmt.Errorf("安全存储写入失败: value=%s rejected", p.Value))
			}
			return errorEnvelopeFrame(req, fmt.Errorf("安全存储写入失败（后端 unavailable）: secrets: 平台安全存储不可用"))
		}
		ref = SecretRefForValue(p.Value)
		f.secrets[ref] = p.Value
		// 与真实 agent 一致：secret.put 会顺手把 ref 写进运行时配置。
		f.settings.SecretRef = ref
		f.mu.Unlock()
		return okEnvelopeFrame(req, map[string]any{"ref": ref, "ok": true})
	case TypeConfigSet:
		var in RuntimeSettings
		if err := decodeRequestEnvelope(req, &in); err != nil {
			return errorEnvelopeFrame(req, err)
		}
		if in.BaseURL == "" {
			return errorEnvelopeFrame(req, fmt.Errorf("provider base_url must not be empty"))
		}
		f.mu.Lock()
		f.setCalls++
		stored := in
		f.setPayloads = append(f.setPayloads, stored)
		// ---- 复刻 agent 的 ApplySettings 合并语义 ----
		next := f.settings
		next.ProviderID = in.ProviderID
		next.ProviderName = in.ProviderName
		next.BaseURL = in.BaseURL
		next.Model = in.Model
		next.WorkspaceRoot = in.WorkspaceRoot
		if in.ContextWindow > 0 {
			next.ContextWindow = in.ContextWindow
		}
		if in.MaxOutputTokens > 0 {
			next.MaxOutputTokens = in.MaxOutputTokens
		}
		if in.SecretRef != "" {
			next.SecretRef = in.SecretRef
		}
		if in.AutoMode != "" {
			next.AutoMode = in.AutoMode
		}
		if in.Providers != nil {
			next.Providers = in.Providers
		}
		if in.MCPServers != nil {
			next.MCPServers = in.MCPServers
		}
		if in.SubAgent.Pool != nil {
			next.SubAgent.Pool = in.SubAgent.Pool
		}
		f.settings = next
		f.mu.Unlock()
		return okEnvelopeFrame(req, map[string]any{"ok": true})
	case TypeModelList:
		f.mu.Lock()
		models := append([]ModelInfo{}, f.modelLists...)
		base := f.settings.BaseURL
		f.mu.Unlock()
		return okEnvelopeFrame(req, ModelList{Models: models, BaseURL: base})
	default:
		// 未注册帧：与主仓库 server.go 一致，回裸文本的 system.error。
		return &Frame{
			Header:  FrameHeader{Version: CurrentVersion, RequestID: req.Header.RequestID, Sequence: req.Header.Sequence + 1, Type: TypeError},
			Payload: []byte(fmt.Sprintf("unknown message type: %s", req.Header.Type)),
		}
	}
}

func (f *fakeAgent) currentSettings() RuntimeSettings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings
}

func (f *fakeAgent) counts() (gets, sets, puts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls, f.setCalls, len(f.putValues)
}

func okEnvelopeFrame(req *Frame, v any) *Frame {
	raw, _ := json.Marshal(v)
	payload, _ := json.Marshal(Envelope{OK: true, Data: raw})
	return &Frame{
		Header: FrameHeader{
			Version:   CurrentVersion,
			RequestID: req.Header.RequestID,
			SessionID: req.Header.SessionID,
			Sequence:  req.Header.Sequence + 1,
			Type:      req.Header.Type,
		},
		Payload: payload,
	}
}

func errorEnvelopeFrame(req *Frame, err error) *Frame {
	payload, _ := json.Marshal(Envelope{OK: false, Error: err.Error()})
	return &Frame{
		Header: FrameHeader{
			Version:   CurrentVersion,
			RequestID: req.Header.RequestID,
			SessionID: req.Header.SessionID,
			Sequence:  req.Header.Sequence + 1,
			Type:      req.Header.Type,
		},
		Payload: payload,
	}
}

func decodeRequestEnvelope(req *Frame, dst any) error {
	var env Envelope
	if len(req.Payload) == 0 {
		return fmt.Errorf("empty request payload")
	}
	if err := json.Unmarshal(req.Payload, &env); err != nil {
		return fmt.Errorf("decode request envelope: %w", err)
	}
	if !env.OK {
		return fmt.Errorf("client sent a failed envelope")
	}
	return env.Decode(dst)
}
