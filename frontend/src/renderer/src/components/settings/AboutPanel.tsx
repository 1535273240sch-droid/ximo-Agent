import { Cpu, MonitorSmartphone, Sparkles } from 'lucide-react'

const APP_VERSION = 'v2.0.0'

const ARCH_FACTS = [
  { label: '引擎内核', value: 'Go 1.27 高并发自愈运行时' },
  { label: '桌面宿主', value: 'Electron 33 + React 18 + Tailwind' },
  { label: '进程协议', value: 'IPC 定长二进制帧 (BigEndian 29B 头)' },
  { label: '持久存储', value: 'SQLite WAL 单写多读事务池 + CAS' },
  { label: '视觉设计', value: 'Obsidian / Graphite / Silver / Warm White / Paper' }
]

export function AboutPanel(): React.JSX.Element {
  return (
    <div className="flex flex-col gap-4 text-[13px]">
      <div className="flex items-center gap-3">
        <div className="flex h-9 w-9 items-center justify-center rounded-[8px] border border-border/[0.1] bg-surface-raised text-accent">
          <Sparkles size={18} />
        </div>
        <div className="flex flex-col">
          <div className="flex items-center gap-2">
            <span className="font-display text-[15px] font-semibold text-ink">XimoAgent</span>
            <span className="font-mono text-[12px] font-medium text-ink-muted">{APP_VERSION}</span>
          </div>
          <span className="text-[12px] text-ink-muted">工业级全能本地智能体工作台</span>
        </div>
      </div>

      <p className="text-[13px] leading-relaxed text-ink-muted">
        XimoAgent Go v2 是基于全新 Go 并发内核构建的智能体系统。采用本地进程看门狗（Supervisor）守护 Engine 核心，
        工具运行时与存储层解耦，支持随机故障下的确定性状态回放与事务级幂等保证。
      </p>

      <div className="rounded-[8px] border border-border/[0.08] bg-canvas p-3.5">
        <dl className="grid grid-cols-1 gap-2 sm:grid-cols-2">
          {ARCH_FACTS.map(({ label, value }) => (
            <div key={label} className="flex flex-col gap-0.5">
              <dt className="text-[12px] font-medium text-ink-faint">{label}</dt>
              <dd className="font-mono text-[12px] font-medium text-ink">{value}</dd>
            </div>
          ))}
        </dl>
      </div>

      <div className="flex items-center gap-3 text-[12px] text-ink-muted pt-1">
        <div className="flex items-center gap-1.5">
          <Cpu size={14} className="text-accent" />
          <span>本地数据驻留 · 隐私安全第一</span>
        </div>
        <span>·</span>
        <div className="flex items-center gap-1.5">
          <MonitorSmartphone size={14} className="text-accent" />
          <span>高质感跨平台桌面体验</span>
        </div>
      </div>
    </div>
  )
}
