package memory

import "testing"

// ParseExtractionJSON 是 provider → Extractor 适配层的解析入口：模型输出里的
// 格式噪声要容忍，语义错误（半截 JSON）绝不放行。

func TestParseExtractionJSONAcceptsFencedOutput(t *testing.T) {
	raw := "好的，以下是抽取结果：\n```json\n" +
		`{"episode":{"title":"修构建","summary":"调通了 CI"},` +
		`"facts":[{"text":"用户偏好深色主题","importance":0.8,"entities":["XimoAgent"]}],` +
		`"procedures":[{"title":"跑测试","steps":"go test ./..."}],` +
		`"relations":[{"from":"用户偏好深色主题","to":"XimoAgent","rel":"related"}]}` +
		"\n```\n希望有帮助。"
	ex, err := ParseExtractionJSON(raw)
	if err != nil {
		t.Fatalf("解析带围栏的输出: %v", err)
	}
	if ex.Episode == nil || ex.Episode.Title != "修构建" {
		t.Fatalf("episode = %+v", ex.Episode)
	}
	if len(ex.Facts) != 1 || ex.Facts[0].Text != "用户偏好深色主题" || ex.Facts[0].Importance != 0.8 {
		t.Fatalf("facts = %+v", ex.Facts)
	}
	if len(ex.Procedures) != 1 || len(ex.Relations) != 1 {
		t.Fatalf("procedures/relations = %+v / %+v", ex.Procedures, ex.Relations)
	}
}

func TestParseExtractionJSONRejectsGarbage(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"我觉得这条记忆挺重要的",
		`{"facts":[{"text":"半截`,
		`{"facts": [}`,
	} {
		if _, err := ParseExtractionJSON(raw); err == nil {
			t.Fatalf("输入 %q 应当解析失败", raw)
		}
	}
}

func TestParseExtractionJSONHandlesBracesInStrings(t *testing.T) {
	ex, err := ParseExtractionJSON(`{"facts":[{"text":"模板写成 {name} 这样","importance":0.5}]} 后面还有解释`)
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(ex.Facts) != 1 || ex.Facts[0].Text != "模板写成 {name} 这样" {
		t.Fatalf("facts = %+v", ex.Facts)
	}
}
