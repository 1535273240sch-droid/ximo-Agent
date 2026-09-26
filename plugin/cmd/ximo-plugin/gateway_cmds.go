package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/1535273240sch-droid/ximo-plugin/internal/gateway"
)

func (a *app) cmdLogin(args []string) int {
	fs := a.flags("login")
	gatewayURL := fs.String("gateway", "", "中转站地址，例 http://127.0.0.1:8600")
	timeout := fs.Duration("timeout", 10*time.Minute, "等待授权的上限")
	if err := fs.Parse(args); err != nil {
		return a.usageErr(err)
	}
	base, err := a.resolveGateway(*gatewayURL)
	if err != nil {
		return a.usageErr(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := newGatewayClient(base, a.home)

	da, err := client.StartDeviceLogin(ctx)
	if err != nil {
		return a.fail(err)
	}
	// 设备码（DeviceAuth.DeviceCode）等价于短期凭据：任何输出都不打印它。
	a.notef("网关：%s", da.GatewayURL)
	a.notef("用户码：%s", da.UserCode)
	if da.VerificationURI != "" {
		a.notef("授权页：%s", da.VerificationURI)
	} else {
		a.notef("请在网关的授权页输入上面的用户码（V1 服务端不返回授权页地址）")
	}
	a.notef("等待授权（每隔 %s 轮询一次，上限 %s，Ctrl+C 可中断）…", da.Interval, *timeout)

	cred, err := client.PollDeviceLogin(ctx, da.DeviceCode)
	if err != nil {
		if errors.Is(err, gateway.ErrLoginTimeout) {
			return a.fail(fmt.Errorf("%w；可重跑本命令重新申请设备码", err))
		}
		return a.fail(err)
	}
	if a.json {
		return a.emitJSON(map[string]any{
			"status":     "ok",
			"gateway":    base,
			"cred_path":  credPath(a.home),
			"credential": cred.Fingerprint(),
		})
	}
	a.passf("登录成功：%s", cred.Fingerprint())
	a.notef("凭据已保存到 %s（0600；P-Client 原子写）", credPath(a.home))
	return exitOK
}

func (a *app) cmdModels(args []string) int {
	fs := a.flags("models")
	gatewayURL := fs.String("gateway", "", "中转站地址")
	apiKey := fs.String("api-key", "", "直接给密钥，跳过本机凭据")
	if err := fs.Parse(args); err != nil {
		return a.usageErr(err)
	}
	base, err := a.resolveGateway(*gatewayURL)
	if err != nil {
		return a.usageErr(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cred, err := a.credFor(ctx, base, *apiKey)
	if err != nil {
		return a.fail(err)
	}
	models, err := newGatewayClient(base, a.home).ListModels(ctx, cred)
	if err != nil {
		return a.fail(err)
	}
	if a.json {
		return a.emitJSON(models)
	}
	if len(models) == 0 {
		a.warnf("网关未返回任何模型（可能未启用模型，或没有可用 provider）")
		return exitOK
	}
	a.notef("网关 %s 提供 %d 个模型：", base, len(models))
	for _, m := range models {
		state := ""
		if !m.Enabled {
			state = " [disabled]"
		}
		switch {
		case m.DisplayName != "" && m.DisplayName != m.ID:
			a.notef("  - %s (%s)%s  provider=%s  protocols=%s", m.ID, m.DisplayName, state, orDash(m.Provider), strings.Join(m.Protocols, ","))
		default:
			a.notef("  - %s%s  provider=%s  protocols=%s", m.ID, state, orDash(m.Provider), strings.Join(m.Protocols, ","))
		}
	}
	return exitOK
}

func (a *app) cmdUsage(args []string) int {
	fs := a.flags("usage")
	gatewayURL := fs.String("gateway", "", "中转站地址")
	apiKey := fs.String("api-key", "", "直接给密钥，跳过本机凭据")
	limit := fs.Int("limit", 20, "返回条数上限（服务端上限 200）")
	if err := fs.Parse(args); err != nil {
		return a.usageErr(err)
	}
	base, err := a.resolveGateway(*gatewayURL)
	if err != nil {
		return a.usageErr(err)
	}
	if *limit <= 0 {
		return a.usageErr(errors.New("--limit 必须为正数"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cred, err := a.credFor(ctx, base, *apiKey)
	if err != nil {
		return a.fail(err)
	}
	records, err := newGatewayClient(base, a.home).ListUsage(ctx, cred, *limit)
	if err != nil {
		return a.fail(err)
	}
	if a.json {
		return a.emitJSON(records)
	}
	if len(records) == 0 {
		a.notef("网关 %s 没有用量记录（最近 %d 条为空）", base, *limit)
		return exitOK
	}
	a.notef("网关 %s 的用量（%d 条，cost 单位 credit）：", base, len(records))
	for _, r := range records {
		when := "-"
		if r.CreatedAt > 0 {
			when = time.UnixMilli(r.CreatedAt).Format("2006-01-02 15:04:05")
		}
		a.notef("  - %s  %s  status=%s  in=%d out=%d  latency=%dms  cost=%.6f  req=%s",
			when, orDash(r.ModelID), orDash(r.Status), r.InputTokens, r.OutputTokens, r.LatencyMS,
			float64(r.CostMicro)/1e6, r.RequestID)
	}
	return exitOK
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
