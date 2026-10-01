package ipcapi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/types"
)

// This file pins the IPC half of the expert catalogue (v2.6.0). The three frames
// are the only way the library page can see the other 194 experts and the only
// way a custom expert can be created — a dropped field or a silent success here
// shows up as "保存了但列表里没有" or "库里明明有 254 位，界面只有 60 位".

// stubDirectory is a scripted types.ExpertDirectoryPort.
type stubDirectory struct {
	list      []types.ExpertCard
	listErr   error
	saved     types.ExpertCard
	saveErr   error
	saveCalls int
	deleted   string
	deleteOK  bool
	deleteErr error
}

func (s *stubDirectory) ListExperts(context.Context) ([]types.ExpertCard, error) {
	return s.list, s.listErr
}

func (s *stubDirectory) SaveExpert(_ context.Context, card types.ExpertCard) (types.ExpertCard, error) {
	s.saveCalls++
	s.saved = card
	return card, s.saveErr
}

func (s *stubDirectory) DeleteExpert(_ context.Context, id string) (bool, error) {
	s.deleted = id
	return s.deleteOK, s.deleteErr
}

// TestExpertListReturnsCatalogueAndDivisions 校验列表帧把目录与部门清单一起返回。
//
// 部门清单由后端算：界面据此渲染筛选条，前端再抄一份部门表就会在新增部门时漏掉。
func TestExpertListReturnsCatalogueAndDivisions(t *testing.T) {
	stub := &stubDirectory{list: []types.ExpertCard{
		{ID: "engineering-frontend-developer", Division: "engineering", Name: "前端开发工程师"},
		{ID: "academic-historian", Division: "academic", Name: "历史学家"},
		{ID: "academic-anthropologist", Division: "academic", Name: "人类学家"},
		{ID: "custom-1", Division: "", Name: "无部门专家"},
	}}
	svc := NewExpertService(stub)

	resp, herr := svc.handleList(context.Background(), memFrame(t, ipc.TypeExpertList, map[string]any{}))
	if herr != nil {
		t.Fatalf("handleList: %v", herr)
	}
	env := decodeResp(t, resp)
	if !env.OK {
		t.Fatalf("response not OK: %s", env.Error)
	}
	var got ExpertListPayload
	if err := env.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 4 || len(got.Experts) != 4 {
		t.Errorf("total/len = %d/%d, want 4/4", got.Total, len(got.Experts))
	}
	// 部门去重且跳过空值（空部门是脏数据，不该成为一个筛选项）。
	if len(got.Divisions) != 2 || got.Divisions[0] != "engineering" || got.Divisions[1] != "academic" {
		t.Errorf("divisions = %v, want [engineering academic]", got.Divisions)
	}
}

// TestExpertListEncodesEmptyCatalogueAsArray 校验空目录编码成 []，而不是 null。
//
// 前端直接 .map 这个字段，null 会在"一个专家都没有"时把整页打崩 —— 而空目录正是
// 全新安装 + 后端尚未加载完时的正常状态。
func TestExpertListEncodesEmptyCatalogueAsArray(t *testing.T) {
	svc := NewExpertService(&stubDirectory{list: nil})

	resp, herr := svc.handleList(context.Background(), memFrame(t, ipc.TypeExpertList, map[string]any{}))
	if herr != nil {
		t.Fatalf("handleList: %v", herr)
	}
	env := decodeResp(t, resp)
	if !env.OK {
		t.Fatalf("response not OK: %s", env.Error)
	}
	var got ExpertListPayload
	if err := env.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 关键：字段本身编码成 []（而不是 null）。断言的是真发给前端的字节，
	// 而不是 Go 侧的切片是否为 nil。
	raw, err := json.Marshal(got.Experts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != "[]" {
		t.Errorf("experts 编码为 %s, want []", raw)
	}
}

// TestExpertServiceWithoutDirectoryFailsLoudly 校验装配缺失时明确报错。
//
// 静默成功会让界面显示"已保存"，而库里什么都没有 —— 用户下次启动就发现专家没了。
func TestExpertServiceWithoutDirectoryFailsLoudly(t *testing.T) {
	svc := NewExpertService(nil)
	for _, tc := range []struct {
		name    string
		msgType string
		call    func(context.Context, *ipc.Frame) (*ipc.Frame, error)
	}{
		{"list", ipc.TypeExpertList, svc.handleList},
		{"save", ipc.TypeExpertSave, svc.handleSave},
		{"delete", ipc.TypeExpertDelete, svc.handleDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, herr := tc.call(context.Background(), memFrame(t, tc.msgType, map[string]any{}))
			if herr != nil {
				t.Fatalf("handler returned a transport error: %v", herr)
			}
			if env := decodeResp(t, resp); env.OK {
				t.Fatal("缺少专家目录时必须返回错误，而不是静默成功")
			}
		})
	}
}

// TestExpertSaveValidation 校验保存前的入参校验：名称、部门必填，超长名称拒绝，
// 且被拒绝的请求**不会**触达存储层。
func TestExpertSaveValidation(t *testing.T) {
	longName := ""
	for i := 0; i < 61; i++ {
		longName += "字"
	}
	cases := []struct {
		name string
		card types.ExpertCard
	}{
		{"空名称", types.ExpertCard{Division: "engineering"}},
		{"空白名称", types.ExpertCard{Name: "   ", Division: "engineering"}},
		{"空部门", types.ExpertCard{Name: "我的专家"}},
		{"名称过长", types.ExpertCard{Name: longName, Division: "engineering"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubDirectory{}
			svc := NewExpertService(stub)
			resp, herr := svc.handleSave(context.Background(),
				memFrame(t, ipc.TypeExpertSave, tc.card))
			if herr != nil {
				t.Fatalf("handleSave: %v", herr)
			}
			if env := decodeResp(t, resp); env.OK {
				t.Fatal("非法入参必须被拒绝")
			}
			if stub.saveCalls != 0 {
				t.Errorf("被拒绝的请求触达了存储层 %d 次", stub.saveCalls)
			}
		})
	}
}

// TestExpertSaveForcesCustomFlag 校验保存路径强制把 Custom 置为 true。
//
// 客户端可以伪造 custom=false；不强制的话注册表会把它当内置专家处理
// （内置不可删、且会顶掉同名内置项），用户建出来的专家就再也不能编辑了。
func TestExpertSaveForcesCustomFlag(t *testing.T) {
	stub := &stubDirectory{}
	svc := NewExpertService(stub)

	resp, herr := svc.handleSave(context.Background(), memFrame(t, ipc.TypeExpertSave, map[string]any{
		"id": "custom-mine", "name": "我的专家", "division": "engineering",
		"custom": false, "tools": []string{"file_read"},
	}))
	if herr != nil {
		t.Fatalf("handleSave: %v", herr)
	}
	if env := decodeResp(t, resp); !env.OK {
		t.Fatalf("response not OK: %s", env.Error)
	}
	if stub.saveCalls != 1 {
		t.Fatalf("存储层被调用 %d 次，期望 1", stub.saveCalls)
	}
	if !stub.saved.Custom {
		t.Error("保存时必须强制 Custom=true")
	}
	if stub.saved.Name != "我的专家" || stub.saved.Division != "engineering" {
		t.Errorf("存储层收到的卡片 = %+v", stub.saved)
	}
	if len(stub.saved.Tools) != 1 || stub.saved.Tools[0] != "file_read" {
		t.Errorf("tools = %v，期望 [file_read]", stub.saved.Tools)
	}
}

// TestExpertDeleteRequiresIDAndReportsNotFound 校验删除帧：缺 id 拒绝；
// 目标不存在（deleted=false）是正常结果而不是错误。
func TestExpertDeleteRequiresIDAndReportsNotFound(t *testing.T) {
	svc := NewExpertService(&stubDirectory{})
	resp, herr := svc.handleDelete(context.Background(),
		memFrame(t, ipc.TypeExpertDelete, ExpertIDPayload{}))
	if herr != nil {
		t.Fatalf("handleDelete: %v", herr)
	}
	if env := decodeResp(t, resp); env.OK {
		t.Fatal("缺少 expert_id 必须被拒绝")
	}

	stub := &stubDirectory{deleteOK: false}
	svc = NewExpertService(stub)
	resp, herr = svc.handleDelete(context.Background(),
		memFrame(t, ipc.TypeExpertDelete, ExpertIDPayload{ExpertID: "nope"}))
	if herr != nil {
		t.Fatalf("handleDelete: %v", herr)
	}
	env := decodeResp(t, resp)
	if !env.OK {
		t.Fatalf("目标不存在不是错误：%s", env.Error)
	}
	var got ExpertDeletePayload
	if err := env.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Deleted {
		t.Error("deleted 应为 false")
	}
	if stub.deleted != "nope" {
		t.Errorf("存储层收到的 id = %q，期望 nope", stub.deleted)
	}
}

// TestExpertDeleteForwardsID 校验 id 原样送达存储层。
func TestExpertDeleteForwardsID(t *testing.T) {
	stub := &stubDirectory{deleteOK: true}
	svc := NewExpertService(stub)
	resp, herr := svc.handleDelete(context.Background(),
		memFrame(t, ipc.TypeExpertDelete, ExpertIDPayload{ExpertID: "custom-x"}))
	if herr != nil {
		t.Fatalf("handleDelete: %v", herr)
	}
	env := decodeResp(t, resp)
	if !env.OK {
		t.Fatalf("response not OK: %s", env.Error)
	}
	var got ExpertDeletePayload
	if err := env.Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Deleted || stub.deleted != "custom-x" {
		t.Errorf("deleted=%v id=%q", got.Deleted, stub.deleted)
	}
}

// TestExpertListSurfacesPortErrors 校验端口报错会变成错误帧，而不是空列表。
//
// 空列表会被界面解读成"目录里一个专家都没有"，比一个明确错误更难排查。
func TestExpertListSurfacesPortErrors(t *testing.T) {
	svc := NewExpertService(&stubDirectory{listErr: errors.New("db is locked")})
	resp, herr := svc.handleList(context.Background(), memFrame(t, ipc.TypeExpertList, map[string]any{}))
	if herr != nil {
		t.Fatalf("handleList: %v", herr)
	}
	if env := decodeResp(t, resp); env.OK {
		t.Fatal("端口报错必须变成错误帧")
	}
}

// TestExpertFrameNamesMatchContract 锁定三个帧名。它们由本服务、Supervisor 的
// 转发器与前端共用，一边改了名字在另一边仍能编译，只会永远不回应答。
func TestExpertFrameNamesMatchContract(t *testing.T) {
	want := map[string]string{
		"list":   "system.expert.list",
		"save":   "system.expert.save",
		"delete": "system.expert.delete",
	}
	got := map[string]string{
		"list": ipc.TypeExpertList, "save": ipc.TypeExpertSave, "delete": ipc.TypeExpertDelete,
	}
	for name, wantVal := range want {
		if got[name] != wantVal {
			t.Errorf("%s frame = %q, want %q", name, got[name], wantVal)
		}
	}
}

// TestExpertCardWireNames pins the field names the frontend reads.
//
// 前端 experts 页与 app-store 都按这些键取值；改名不会编译失败，只会让界面永远
// 显示空值（专家名、部门、工具全部为空）。
func TestExpertCardWireNames(t *testing.T) {
	raw, err := json.Marshal(types.ExpertCard{
		ID: "e1", Division: "engineering", Name: "前端", Description: "d",
		Emoji: "🖥️", Vibe: "v", Personality: "p", Color: "cyan",
		Tools: []string{"file_read"}, Custom: true,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"id", "division", "name", "description", "emoji", "vibe",
		"personality", "color", "tools", "custom",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("ExpertCard JSON 缺少字段 %q（前端按这个名字读）", key)
		}
	}
}
