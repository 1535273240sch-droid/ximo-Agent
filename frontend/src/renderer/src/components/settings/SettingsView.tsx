import { Info, Key, Palette, Server, Shield, Users } from 'lucide-react'
import { ProviderPanel } from './ProviderPanel'
import { SubAgentModelPanel } from './SubAgentModelPanel'
import { PermissionPanel } from './PermissionPanel'
import { ThemePicker } from './ThemePicker'
import { BackendPanel } from './BackendPanel'
import { AboutPanel } from './AboutPanel'

/**
 * 设置页。
 *
 * 极简奢华结构设计，包含：
 *   1. 模型服务商与 API 凭证管理 (系统级加密，绝不回显)
 *   2. 外观主题 (黑曜石 / 石墨 / 银白 / 暖白 / 原纸 五款高定主题)
 *   3. 本地 Go 引擎运行宿主状态
 *   4. 关于与版本信息
 */
export function SettingsView(): React.JSX.Element {
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex max-w-[800px] flex-col gap-5 px-6 pb-12 pt-6">
        <header className="shrink-0">
          <h1 className="font-display text-[22px] font-semibold tracking-tight text-ink">系统设置</h1>
          <p className="mt-1 text-[13px] text-ink-muted">模型连接、外观视觉与本地引擎管理</p>
        </header>

        <Section
          icon={<Key size={15} className="text-accent" />}
          title="模型与 API 凭据"
          hint="配置第三方大模型接口地址与调用凭据，敏感凭证受 OS 钥匙串硬件级隔离保护"
        >
          <ProviderPanel />
        </Section>

        <Section
          icon={<Users size={15} className="text-accent" />}
          title="子代理模型分配"
          hint="给并行执行的专家子代理配置候选模型池：限流或断线时自动换下一个候选继续任务，并按专家分类分配不同模型"
        >
          <SubAgentModelPanel />
        </Section>

        <Section
          icon={<Shield size={15} className="text-accent" />}
          title="权限模式"
          hint="控制 Agent 使用工具时的放权程度：从逐项确认到完全放权；已写入配置文件，重启应用后生效"
        >
          <PermissionPanel />
        </Section>

        <Section
          icon={<Palette size={15} className="text-accent" />}
          title="视觉主题"
          hint="支持 5 款极简高对比度主题，点击即刻生效并持久化"
        >
          <ThemePicker />
        </Section>

        <Section
          icon={<Server size={15} className="text-accent" />}
          title="本地 Go 引擎宿主"
          hint="独立并发 Go 后端守护进程，支持崩溃自愈与跨会话状态回放"
        >
          <BackendPanel />
        </Section>

        <Section icon={<Info size={15} className="text-accent text-[13px]" />} title="关于 XimoAgent">
          <AboutPanel />
        </Section>
      </div>
    </div>
  )
}

interface SectionProps {
  icon: React.ReactNode
  title: string
  hint?: string
  children: React.ReactNode
}

function Section({ icon, title, hint, children }: SectionProps): React.JSX.Element {
  return (
    <section className="glass-panel p-5">
      <div className="flex items-center gap-2">
        {icon}
        <h2 className="font-display text-[15px] font-semibold text-ink">
          {title}
        </h2>
      </div>
      {hint && <p className="mt-1 text-[12px] text-ink-muted leading-relaxed">{hint}</p>}
      <div className="mt-4">{children}</div>
    </section>
  )
}
