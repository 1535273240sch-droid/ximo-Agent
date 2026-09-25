package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ximo888ok-netizen/ximo-agent/internal/bootstrap"
	"github.com/ximo888ok-netizen/ximo-agent/internal/config"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipc"
	"github.com/ximo888ok-netizen/ximo-agent/internal/ipcapi"
	"github.com/ximo888ok-netizen/ximo-agent/internal/supervisor"
)

var (
	version   = "v2.0.0-alpha"
	gitCommit = "dev"
	buildTime = "unknown"
)

func main() {
	var (
		roleFlag        string
		kindFlag        string
		configPath      string
		ipcEndpointFlag string
		showVersion     bool
		healthCheck     bool
	)

	flag.StringVar(&roleFlag, "role", "supervisor", "process role: supervisor | ui | engine | worker")
	flag.StringVar(&kindFlag, "kind", "", "worker kind (when role=worker): browser | dynamic-js | mcp | terminal | office | computer-use")
	flag.StringVar(&configPath, "config", "", "path to configuration file")
	flag.StringVar(&ipcEndpointFlag, "ipc-endpoint", "", "ipc endpoint address (named pipe or unix domain socket)")
	flag.BoolVar(&showVersion, "version", false, "display version and build information")
	flag.BoolVar(&healthCheck, "health", false, "run version health check (used by upgrade scripts)")

	flag.Parse()

	if showVersion {
		fmt.Printf("XimoAgent %s (commit: %s, built: %s)\n", version, gitCommit, buildTime)
		return
	}

	// 未显式指定 --config 时，自动加载 <BaseDir>/config.json（设置界面
	// 保存的持久化配置）。此前引擎启动永远拿默认配置：用户保存的服务商
	// 设置一重启就"丢失"，模型调用始终打在出厂默认地址上。
	if configPath == "" {
		if candidate := config.DefaultConfigPath(); candidate != "" {
			if _, err := os.Stat(candidate); err == nil {
				configPath = candidate
			}
		}
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		log.Fatalf("[FATAL] Load config failed: %v", err)
	}
	if configPath != "" {
		log.Printf("[CONFIG] Loaded configuration from %s", configPath)
	}

	endpoint := ipcEndpointFlag
	if endpoint == "" {
		endpoint = ipc.FormatEndpoint("ximo-agent-ipc")
	}

	if healthCheck {
		vm := config.NewVersionManager(cfg.Paths)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := vm.VerifyHealth(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Health check failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Health check OK")
		return
	}

	switch roleFlag {
	case "supervisor":
		runSupervisor(cfg, endpoint)
	case "engine":
		runEngine(cfg, endpoint)
	case "worker":
		runWorker(cfg, endpoint, kindFlag)
	case "ui":
		runUIHost(cfg, endpoint)
	default:
		log.Fatalf("[FATAL] Unknown role: %s", roleFlag)
	}
}

func runSupervisor(cfg *config.Config, endpoint string) {
	log.Printf("[SUPERVISOR] Starting XimoAgent Supervisor on %s...", endpoint)
	_ = cfg.Paths.EnsureDirectories()

	ipcServer := ipc.NewServer(endpoint, cfg.IPC.MaxPayloadLength)

	// 业务帧代理：Supervisor 只做进程监管，业务真相在 Engine 进程里
	// （数据库、密钥库、模型调用）。因此把未在本服务端注册的帧转发给 Engine，
	// 而不是回 "unknown method"——否则界面上所有依赖 Engine 的能力都会失效。
	forwarder := supervisor.NewEngineForwarder(deriveEngineEndpoint(endpoint), cfg.IPC.MaxPayloadLength)
	defer func() { _ = forwarder.Close() }()
	ipcServer.SetForwarder(forwarder)

	if err := ipcServer.Start(); err != nil {
		log.Fatalf("[SUPERVISOR] Failed to start IPC server: %v", err)
	}
	defer ipcServer.Stop()

	// 真实的恢复器：Engine 子进程被重新拉起后，通过 IPC 下发
	// engine.run.resume 帧，由 Engine 进程侧扫描持久化日志并继续未完成的 run。
	// 此前这里用的是 supervisor.MockEngineResumer{}（什么都不做），
	// 导致「kill Engine 自动恢复」这条 P0 红线在进程级别从未真正接通。
	var frameSeq atomic.Uint64
	resumer := supervisor.NewIPCResumer(
		supervisor.NewServerBroadcastResumer(ipcServer, func() uint64 { return frameSeq.Add(1) }),
	)
	sup := supervisor.NewSupervisor(cfg.Supervisor, cfg.Paths.CrashDumpDir, resumer)
	sup.SetIPCServer(ipcServer)

	// 注册子进程管理 (Engine)
	selfBinary, _ := os.Executable()
	engineProc := supervisor.NewManagedProcess(
		selfBinary,
		[]string{"--role=engine", fmt.Sprintf("--ipc-endpoint=%s", endpoint)},
		nil,
		cfg.Paths.BaseDir,
	)

	engineEntity := &processSupervisable{
		id:   "engine-main",
		kind: "engine",
		proc: engineProc,
	}

	_ = sup.RegisterEntity(engineEntity, engineProc)

	if err := sup.Start(); err != nil {
		log.Fatalf("[SUPERVISOR] Failed to start watchdog: %v", err)
	}

	log.Printf("[SUPERVISOR] Supervisor is running. Waiting for termination signals...")

	// 监听系统信号进行第29章的9步优雅停机
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	log.Printf("[SUPERVISOR] Received termination signal. Initiating 9-step graceful shutdown...")

	if err := sup.GracefulShutdown(); err != nil {
		log.Printf("[SUPERVISOR] Graceful shutdown encountered error: %v", err)
	} else {
		log.Printf("[SUPERVISOR] Graceful shutdown completed cleanly.")
	}
}

// runEngine 是 Engine 角色的生产入口：装配真实的 Engine 并对外提供 IPC 服务。
//
// 这是本次补齐的核心。此前的实现只做「连上 Supervisor + 每 2 秒发心跳」，
// 从未创建 Engine 实例——也就是说那个可执行文件里没有数据库、没有模型调用、
// 没有工具、没有 Agent 循环，整个后端模块群从未被链接进任何二进制。
func runEngine(cfg *config.Config, endpoint string) {
	log.Printf("[ENGINE] Starting XimoAgent Engine. Connecting to Supervisor on %s...", endpoint)

	// 1. 装配全部真实依赖（SQLite / Provider / 工具运行时 / Worker 池 / Agent 循环）。
	app, err := bootstrap.New(cfg, bootstrap.Options{})
	if err != nil {
		log.Fatalf("[ENGINE] Failed to assemble engine: %v", err)
	}
	defer app.Close()
	log.Printf("[ENGINE] Engine assembled (db=%s, model=%s, auto_mode=%s)",
		cfg.ResolveDBPath(), cfg.Provider.Model, cfg.Runtime.AutoMode)

	// 2. 启动本地 IPC 服务端，接受 Supervisor 与 UI 的业务帧。
	//
	//    端点派生：Supervisor 与 Engine 在同一台机器上各需一个监听端点，
	//    因此不能同名。派生方式必须按端点类型区分——tcp:// 端点要替换端口号
	//    （追加后缀会变成非法的 "32999-engine" 端口），而管道/socket 端点
	//    直接改名字即可。
	engineEndpoint := deriveEngineEndpoint(endpoint)
	srv := ipc.NewServer(engineEndpoint, cfg.IPC.MaxPayloadLength)
	if err := srv.Start(); err != nil {
		log.Fatalf("[ENGINE] Failed to start engine IPC server on %s: %v", engineEndpoint, err)
	}
	defer srv.Stop()

	svc, err := ipcapi.NewEngineService(app.Engine)
	if err != nil {
		log.Fatalf("[ENGINE] Failed to build IPC service: %v", err)
	}
	svc.Register(srv)

	// 计划模式（任务4）：用户点「确认执行 / 重新规划」走这条帧。
	// 引擎实现了 ConfirmPlan，这里把它注册成可选能力；未实现时该帧返回明确
	// 错误而不是静默成功。
	if confirmer, ok := any(app.Engine).(ipcapi.PlanConfirmer); ok {
		ipcapi.NewPlanService(confirmer).Register(srv)
	} else {
		log.Printf("[ENGINE] Warning: engine does not support plan confirmation; plan mode will not be confirmable")
	}

	// 密钥与配置管理（第21章）：这是界面上"填第三方 API 密钥"的后端入口。
	// 密钥只写不读——写入即交给平台安全存储（Windows DPAPI），查询只回状态。
	admin := ipcapi.NewAdminService(app.Secrets(), app)
	admin.Register(srv)

	// 3. 启动时先自行恢复一次：即使 Supervisor 没发 resume 帧（例如 Engine 是
	//    被手动启动的），崩溃前未完成的 run 也应当被继续推进。
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 30*time.Second)
	plans, err := app.Engine.Recover(recoverCtx)
	recoverCancel()
	if err != nil {
		log.Printf("[ENGINE] Startup recovery failed: %v", err)
	} else if len(plans) > 0 {
		log.Printf("[ENGINE] Startup recovery scanned %d run(s)", len(plans))
	}

	// 4. 连接到 Supervisor 上报心跳，并按 Supervisor 的恢复指令执行恢复。
	client := ipc.NewClient(endpoint, cfg.IPC.MaxPayloadLength, true)
	connCtx, cancelConn := context.WithTimeout(context.Background(), 5*time.Second)
	if err := client.Connect(connCtx); err != nil {
		cancelConn()
		// 连不上 Supervisor 不应让 Engine 直接死掉：Engine 本身是可独立工作的，
		// 而且 Supervisor 会负责重启进程。这里降级为「无监督运行」。
		log.Printf("[ENGINE] Warning: cannot reach supervisor at %s (%v); running unsupervised",
			endpoint, err)
	} else {
		cancelConn()
		defer client.Close()

		client.Subscribe(ipc.TypeRunResume, func(f *ipc.Frame) {
			log.Printf("[ENGINE] Received RunResume command from supervisor")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			recovered, rerr := app.Engine.RecoverForIPC(ctx)
			if rerr != nil {
				log.Printf("[ENGINE] Recovery triggered by supervisor failed: %v", rerr)
				return
			}
			log.Printf("[ENGINE] Recovery complete: %d plan(s) evaluated", len(recovered))
		})
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	ticker := time.NewTicker(cfg.Supervisor.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-sigChan:
			log.Printf("[ENGINE] Stopping engine...")
			return
		case <-ticker.C:
			_ = client.Ping(1 * time.Second)
		}
	}
}

func runWorker(cfg *config.Config, endpoint string, kind string) {
	if kind == "" {
		kind = "generic"
	}
	log.Printf("[WORKER] Starting Worker [%s]. Connecting to Supervisor on %s...", kind, endpoint)

	client := ipc.NewClient(endpoint, cfg.IPC.MaxPayloadLength, true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := client.Connect(ctx); err != nil {
		cancel()
		log.Fatalf("[WORKER:%s] Connect to IPC failed: %v", kind, err)
	}
	cancel()
	defer client.Close()

	ticker := time.NewTicker(cfg.Supervisor.HeartbeatInterval)
	defer ticker.Stop()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-sigChan:
			log.Printf("[WORKER:%s] Stopping worker...", kind)
			return
		case <-ticker.C:
			_ = client.Ping(1 * time.Second)
		}
	}
}

// runUIHost 是 UI 角色的入口（第26章）。
//
// 当前它做两件真实的事，而不是只占位：
//  1. 连接到 Engine 的 IPC 端点并探活，确认「UI 能连上后端」；
//  2. 断开时按帧协议重连。
//
// 真正的图形界面属于前端阶段（Wails/React），届时本函数会被替换为
// 「启动 Wails 窗口 + 把 ipcapi.Client 注入前端」；在此之前，这条路径的存在
// 让「前后端能对话」这件事至少有一个可验证的后端侧锚点。
func runUIHost(cfg *config.Config, endpoint string) {
	log.Printf("[UI] Starting UI Host. Connecting to Supervisor on %s...", endpoint)

	client := ipcapi.NewClient(endpoint, cfg.IPC.MaxPayloadLength, cfg.IPC.DefaultTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := client.Connect(ctx); err != nil {
		cancel()
		log.Fatalf("[UI] Connect to IPC failed: %v", err)
	}
	cancel()
	defer client.Close()

	if err := client.Ping(); err != nil {
		log.Printf("[UI] Warning: supervisor did not answer ping: %v", err)
	} else {
		log.Printf("[UI] Connected to backend and verified liveness.")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	<-sigChan
	log.Printf("[UI] UI Host exited.")
}

type processSupervisable struct {
	id   string
	kind string
	proc *supervisor.ManagedProcess
}

func (p *processSupervisable) ID() string   { return p.id }
func (p *processSupervisable) Kind() string { return p.kind }

func (p *processSupervisable) Start() error {
	return p.proc.Start()
}

func (p *processSupervisable) Heartbeat() error {
	if !p.proc.IsAlive() {
		return fmt.Errorf("process not alive (PID: %d)", p.proc.PID())
	}
	return nil
}

func (p *processSupervisable) Kill() error {
	return p.proc.Kill()
}

// deriveEngineEndpoint 为 Engine 进程派生一个不与 Supervisor 冲突的监听端点。
//
// tcp:// 端点必须替换端口号：直接追加后缀会生成 "tcp://host:32999-engine"，
// 这不是合法的端口，net.Listen 会报 "unknown port"。管道与 unix socket 端点
// 只需改名字。
func deriveEngineEndpoint(endpoint string) string {
	const tcpPrefix = "tcp://"
	if !strings.HasPrefix(endpoint, tcpPrefix) {
		return endpoint + "-engine"
	}

	hostPort := endpoint[len(tcpPrefix):]
	idx := strings.LastIndex(hostPort, ":")
	if idx < 0 {
		// 没有端口号（异常输入）：退化成不做变形的原名，让 Listen 自己报错，
		// 比这里静默编造一个地址更容易排查。
		return endpoint
	}
	host, portStr := hostPort[:idx], hostPort[idx+1:]
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return endpoint
	}
	return fmt.Sprintf("%s%s:%d", tcpPrefix, host, port+1)
}
