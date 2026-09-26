package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/1535273240sch-droid/ximo-plugin/internal/engine"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

func (a *app) cmdDetect(args []string) int {
	fs := a.flags("detect")
	showAll := fs.Bool("all", false, "连未命中的适配器及其证据一起打印")
	if err := fs.Parse(args); err != nil {
		return a.usageErr(err)
	}
	specs, err := loadSpecsFn(a.home)
	if err != nil {
		return a.fail(fmt.Errorf("加载适配器规格失败: %w", err))
	}
	dets := engine.DetectIn(specs, a.expand())
	found := engine.Found(dets)

	if a.json {
		return a.emitJSON(map[string]any{"home": a.home, "detected": found, "all": dets})
	}
	if len(specs) == 0 {
		a.warnf("没有加载到任何适配器规格（内置规格库为空，或 --home 指向了空目录）")
		return exitOK
	}
	if len(found) == 0 {
		a.notef("未检测到任何 Agent —— 这不代表出错，常见原因是这些工具不在 PATH 上或本机未安装。")
		a.notef("  - 查看全部内置适配器：ximo-plugin adapters list")
		a.notef("  - 放自定义规格：%s", filepath.Join(a.home, ".ximo-plugin", "specs"))
		if *showAll {
			a.notef("")
			a.printDetections(dets)
		} else {
			a.notef("（用 `ximo-plugin detect --all` 可查看全部适配器的证据）")
		}
		return exitOK
	}
	a.notef("检测到 %d 个可接入的 Agent：", len(found))
	a.printDetections(found)
	if rest := len(dets) - len(found); rest > 0 {
		if *showAll {
			a.notef("未命中的适配器：")
			var miss []engine.Detection
			for _, d := range dets {
				if !d.Found {
					miss = append(miss, d)
				}
			}
			a.printDetections(miss)
		} else {
			a.notef("（另有 %d 个适配器未命中，用 `ximo-plugin detect --all` 看证据）", rest)
		}
	}
	return exitOK
}

func (a *app) printDetections(dets []engine.Detection) {
	for _, d := range dets {
		mark := "[-]"
		if d.Found {
			mark = "[+]"
		}
		a.notef("%s %s (%s)", mark, d.Name, d.SpecID)
		for _, ev := range d.Evidence {
			hit := "miss"
			if ev.OK {
				hit = "hit "
			}
			switch {
			case ev.Resolved != "" && ev.Detail != "":
				a.notef("      %s %-6s %s -> %s  [%s]", hit, ev.Kind, ev.Need, ev.Resolved, ev.Detail)
			case ev.Resolved != "":
				a.notef("      %s %-6s %s -> %s", hit, ev.Kind, ev.Need, ev.Resolved)
			default:
				a.notef("      %s %-6s %s  [%s]", hit, ev.Kind, ev.Need, ev.Detail)
			}
		}
		if len(d.Evidence) == 0 {
			a.notef("      (规格未声明 detect，无法自动判定)")
		}
	}
}

func (a *app) cmdAdapters(args []string) int {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "list":
		fs := a.flags("adapters list")
		if err := fs.Parse(args); err != nil {
			return a.usageErr(err)
		}
		specs, err := loadSpecsFn(a.home)
		if err != nil {
			return a.fail(fmt.Errorf("加载适配器规格失败: %w", err))
		}
		if a.json {
			return a.emitJSON(specs)
		}
		if len(specs) == 0 {
			a.warnf("没有加载到任何适配器规格")
			return exitOK
		}
		a.notef("共 %d 个适配器（内置 + %s）：", len(specs), filepath.Join(a.home, ".ximo-plugin", "specs"))
		a.notef("%-32s %-22s %-20s %s", "ID", "名称", "协议", "改文件/环境变量")
		for _, s := range specs {
			files := 0
			for _, f := range s.Files {
				if f.Path != "" {
					files++
				}
			}
			env := 0
			for _, v := range []string{s.Env.BaseURL, s.Env.APIKey, s.Env.Model} {
				if strings.TrimSpace(v) != "" {
					env++
				}
			}
			env += len(s.Env.Extra)
			protos := strings.Join(s.Protocols, ",")
			if protos == "" {
				protos = "-"
			}
			a.notef("%-32s %-22s %-20s %d 个文件 / %d 个变量", s.ID, s.Name, protos, files, env)
		}
		a.notef("")
		a.notef("查看某个适配器的完整规格：ximo-plugin adapters show <id>")
		return exitOK
	case "show":
		fs := a.flags("adapters show")
		if err := fs.Parse(args); err != nil {
			return a.usageErr(err)
		}
		rest := fs.Args()
		if len(rest) != 1 {
			return a.usageErr(fmt.Errorf("用法: ximo-plugin adapters show <id>"))
		}
		specs, err := loadSpecsFn(a.home)
		if err != nil {
			return a.fail(fmt.Errorf("加载适配器规格失败: %w", err))
		}
		s, ok := engine.FindSpec(specs, rest[0])
		if !ok {
			var ids []string
			for _, x := range specs {
				ids = append(ids, x.ID)
			}
			return a.fail(fmt.Errorf("没有 id=%q 的适配器（可用：%s）", rest[0], strings.Join(ids, ", ")))
		}
		if a.json {
			return a.emitJSON(s)
		}
		printSpec(a, s)
		return exitOK
	default:
		return a.usageErr(fmt.Errorf("未知子命令 adapters %s（可选 list|show）", sub))
	}
}

func printSpec(a *app, s spec.Spec) {
	b, err := marshalIndent(s)
	if err != nil {
		a.fail(fmt.Errorf("序列化规格失败: %w", err))
		return
	}
	a.notef("%s", string(b))
}
