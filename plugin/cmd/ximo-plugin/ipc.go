package main

import (
	"github.com/1535273240sch-droid/ximo-plugin/internal/agents"
	"github.com/1535273240sch-droid/ximo-plugin/internal/engine"
	"github.com/1535273240sch-droid/ximo-plugin/internal/spec"
)

// ximoAgentSpecID 是唯一需要代码适配（IPC 帧）的适配器，见契约 §5 与
// internal/spec/specs/ximo-agent.json：优先 IPC，配置文件只是回退路径。
const ximoAgentSpecID = "ximo-agent"

// ipcEligible 判断某个适配器本次是否走 IPC 帧。只有 ximo-agent 需要代码适配，
// 其余适配器都是「改文件 / 提示环境变量」。
//
// 端点按 flag → $XIMO_IPC_ENDPOINT → 平台默认 的顺序发现（与 agents.DiscoverEndpoint
// 一致）。只有「本平台确实有默认端点」（Windows 命名管道）或「用户显式指定了端点」
// 才认为走 IPC：其它平台上硬试一个不存在的默认端点，只会给每次 apply 添一条连接
// 失败告警，随后还是要回退改文件。
func (a *app) ipcEligible(s spec.Spec, noIPC bool) bool {
	if noIPC || s.ID != ximoAgentSpecID {
		return false
	}
	endpoint, source := agents.DiscoverEndpoint(a.ipcEndpoint)
	if endpoint == "" {
		return false
	}
	return source != agents.EndpointFromPlatform || defaultIPCEndpoint() != ""
}

// applyXimoAgentIPC 把 CLI 的输入交给 agents.ApplyXimoAgent（P-AgentIPC 的权威实现）。
//
// 为什么不再自己拼载荷：主仓库 ApplySettings 的合并语义里，providers / mcp_servers
// 为 nil 才表示「未携带，沿用现有配置」；把 system.config.get 的结果整体回传会触发
// 「整体替换」，用一份快照顶掉用户自己配好的候选池与 MCP 清单。备份、dry-run 差异、
// 安全存储不可用时的逐条告警、以及「只回传该回传的字段」（PatchForEcho）都在 agents
// 那一侧，这里只负责把 CLI 的取值搬过去。
//
// configPath 是规格解析出的目标文件路径，用于 IPC 不可用时的文件回退落点
// （见 ximoAgentFallbackPath）。agents 的 IPC 路径会优先用它做写前备份——真机上
// 规格声明的路径就是 agent 自己的 config.json（同一个文件），因此两者一致；只有
// 用户把规格改得与 agent 实际路径不符时才会以规格为准，这是刻意的：apply 打印的
// 计划与实际落点必须是同一处。
func (a *app) applyXimoAgentIPC(in engine.Inputs, configPath, apiKey string, dryRun bool) error {
	cfg := agents.Config{
		// 传规整过的 base_url（裸主机补 /v1）：网关只注册 POST /v1/chat/completions，
		// 而 ximo-agent 的 provider 是把请求发到 <base_url>/chat/completions 的
		// （主仓库 internal/provider/openai.go:236）。agents 自己的 normalizeBaseURL
		// 只补协议头、不补 /v1，所以这里必须先过 engine.GatewayBaseURL——它同时是
		// 文件路径写进去的那个值，两条路径必须写同一个 base_url。
		GatewayURL: engine.GatewayBaseURL(in.GatewayURL),
		Model:      in.Model,
		// provider_id 固定为 ximo-gateway（与规格 files[0] 里 provider.id 的
		// literal 一致）；provider_name 沿用手写时代的行为：跟着模型名走。
		ProviderID:  "ximo-gateway",
		DisplayName: in.Model,
		// 注意这里传的是真实密钥 key（可能为空），不是 in.APIKey：后者在
		// dry-run 且没有可用密钥时是 dryRunKeyPlaceholder 占位符，不是凭据。
		// 密钥明文只经 agents 的 system.secret.put 进平台安全存储，绝不落配置文件。
		APIKey:     apiKey,
		Endpoint:   a.ipcEndpoint,
		ConfigPath: configPath,
		Home:       a.home,
		DryRun:     dryRun,
		Output:     a.noteWriter(),
	}
	return agents.ApplyXimoAgent(cfg)
}
