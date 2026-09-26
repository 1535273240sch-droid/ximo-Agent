// Package engine 把「适配器规格」变成对某台机器的具体变更：检测环境、生成变更计划、执行变更。
// 它不读网络、不读凭据存储，所有外部效果都集中在 Apply。
package engine

import (
	"os"
	"os/exec"
	"strings"

	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// 证据种类（检测命中的理由）。
const (
	EvidenceBinary = "binary"
	EvidencePath   = "path"
	EvidenceEnv    = "env"
)

// Evidence 是一条检测证据：规格里声明了什么、实际解析成什么、是否命中。
type Evidence struct {
	Kind     string `json:"kind"`
	Need     string `json:"need"`
	Resolved string `json:"resolved,omitempty"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail,omitempty"`
}

// Detection 是一个适配器在本机的检测结果。Found 为真表示任一条证据命中。
type Detection struct {
	SpecID   string     `json:"spec_id"`
	Name     string     `json:"name"`
	Found    bool       `json:"found"`
	Evidence []Evidence `json:"evidence"`
}

// Detect 按每个 spec 的 detect 声明探测本机；home 用于展开 ~。
// 顺序与入参一致，便于稳定输出。没有任何 detect 声明的 spec 会返回空证据且 Found=false。
func Detect(specs []spec.Spec, home string) []Detection {
	return DetectIn(specs, spec.ExpandOptions{Home: home})
}

// DetectIn 与 Detect 相同，但接受完整的展开上下文（CLI --home 的隔离语义）。
// 路径展开只有 spec.ExpandPathOpts 一份实现：引用未定义变量时给出错误证据，
// 而不是静默展开成空串（后者会让证据里出现 "/ximo-agent/config.json" 这种噪声，
// 还会把「本该报错」伪装成 miss）。
func DetectIn(specs []spec.Spec, opt spec.ExpandOptions) []Detection {
	out := make([]Detection, 0, len(specs))
	for _, s := range specs {
		d := Detection{SpecID: s.ID, Name: s.Name}
		for _, b := range s.Detect.Binaries {
			ev := Evidence{Kind: EvidenceBinary, Need: b}
			p, err := exec.LookPath(b)
			if err != nil {
				ev.Detail = "PATH 上未找到"
			} else {
				ev.Resolved = p
				ev.OK = true
			}
			d.Evidence = append(d.Evidence, ev)
		}
		for _, raw := range s.Detect.Paths {
			ev := Evidence{Kind: EvidencePath, Need: raw}
			resolved, err := spec.ExpandPathOpts(raw, opt)
			switch {
			case err != nil:
				ev.Detail = "路径无法展开: " + err.Error()
			default:
				ev.Resolved = resolved
				if _, err := os.Stat(resolved); err != nil {
					ev.Detail = err.Error()
				} else {
					ev.OK = true
				}
			}
			d.Evidence = append(d.Evidence, ev)
		}
		for _, name := range s.Detect.Env {
			ev := Evidence{Kind: EvidenceEnv, Need: name}
			if os.Getenv(name) != "" {
				ev.OK = true
				ev.Detail = "已设置"
			} else {
				ev.Detail = "未设置"
			}
			d.Evidence = append(d.Evidence, ev)
		}
		for _, e := range d.Evidence {
			if e.OK {
				d.Found = true
				break
			}
		}
		out = append(out, d)
	}
	return out
}

// Found 过滤出命中的检测结果。
func Found(dets []Detection) []Detection {
	var out []Detection
	for _, d := range dets {
		if d.Found {
			out = append(out, d)
		}
	}
	return out
}

// FindSpec 按 ID 查找规格；第二个返回值为是否找到。
func FindSpec(specs []spec.Spec, id string) (spec.Spec, bool) {
	for _, s := range specs {
		if s.ID == id {
			return s, true
		}
	}
	return spec.Spec{}, false
}

// MaskKey 只保留密钥头尾，用于展示；长度过短时全部打码，绝不明文。
func MaskKey(k string) string {
	switch {
	case k == "":
		return ""
	case len(k) <= 8:
		return strings.Repeat("*", len(k))
	default:
		return k[:4] + strings.Repeat("*", 8) + k[len(k)-4:]
	}
}
