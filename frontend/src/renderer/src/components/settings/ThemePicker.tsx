import { Check } from 'lucide-react'
import { THEMES, THEME_META, type ThemeId } from '../../themes/tokens'
import { useStore } from '../../store/app-store'

/**
 * 把主题令牌写成 CSS 自定义属性，返回局部 style 对象，用于在卡片内实时渲染真实预览。
 */
function themeVars(id: ThemeId): React.CSSProperties {
  const t = THEMES[id] || THEMES['obsidian']
  const vars: Record<string, string> = {
    '--c-canvas': t.canvas,
    '--c-surface': t.surface,
    '--c-surface-raised': t.surfaceRaised,
    '--c-border': t.border,
    '--c-border-alpha': String(t.borderAlpha),
    '--c-ink': t.ink,
    '--c-ink-muted': t.inkMuted,
    '--c-ink-faint': t.inkFaint,
    '--c-accent': t.accent,
    '--c-accent-ink': t.accentInk,
    '--c-danger': t.danger,
    '--c-success': t.success,
    '--c-warning': t.warning,
    '--font-ui': t.fontUi,
    '--font-display': t.fontDisplay,
    '--font-mono': t.fontMono,
    '--radius-main': t.radius,
    '--shadow-subtle': t.shadowSubtle,
    '--shadow-card': t.shadowCard
  }
  return vars as React.CSSProperties
}

/**
 * 极简奢华主题选择器 (支持 Obsidian, Graphite, Silver, Warm White, Paper 五款主题)。
 */
export function ThemePicker(): React.JSX.Element {
  const theme = useStore((s) => s.theme)
  const setTheme = useStore((s) => s.setTheme)

  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {THEME_META.map((meta) => {
        const active = meta.id === theme
        return (
          <button
            key={meta.id}
            type="button"
            onClick={() => setTheme(meta.id)}
            aria-pressed={active}
            className={[
              'flex flex-col gap-3 p-4 text-left transition-all duration-150 rounded-panel',
              active ? 'glass-raised border-accent/40' : 'glass-panel glass-panel-hover'
            ].join(' ')}
          >
            <div className="flex items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <span className="font-display text-[14px] font-semibold text-ink">
                  {meta.name}
                </span>
                <span className="font-mono text-[12px] text-ink-faint">
                  {meta.subtitle}
                </span>
              </div>
              {active ? (
                <span className="flex items-center gap-1 rounded-full border border-accent/40 bg-accent/15 px-2 py-0.5 text-[12px] font-medium text-accent">
                  <Check size={11} />
                  当前使用
                </span>
              ) : (
                <span className="text-[12px] text-ink-faint">点击应用</span>
              )}
            </div>

            {/* 真实渲染的主题缩略模拟 */}
            <div
              style={themeVars(meta.id)}
              className="bg-canvas relative overflow-hidden rounded-[8px] border border-border/[0.12] p-3 text-[12.5px]"
            >
              <div className="flex flex-col gap-2">
                <div className="glass-panel px-3 py-2 text-ink">
                  <p className="font-medium">精准极简架构已就绪</p>
                </div>
                <div className="glass-raised ml-3 px-3 py-1.5 text-ink-muted">
                  <p>分析当前系统日志中</p>
                </div>
                <div className="mt-1 flex items-center gap-2">
                  <span className="glass-accent-solid px-3 py-1 text-[12px] font-medium">
                    发送指令
                  </span>
                  <span className="h-2 w-2 rounded-full bg-success" />
                  <span className="text-[12px] text-ink-faint">0.12s 毫秒延迟</span>
                </div>
              </div>
            </div>

            {/* 色卡与描述 */}
            <div className="flex flex-col gap-1.5 pt-1">
              <div className="flex items-center gap-2">
                <span className="flex shrink-0 overflow-hidden rounded-[4px] border border-border/[0.15]">
                  {meta.swatch.map((hex) => (
                    <span
                      key={hex}
                      className="h-3.5 w-4"
                      style={{ backgroundColor: hex }}
                    />
                  ))}
                </span>
                <span className="text-[12px] font-normal leading-normal text-ink-muted">
                  {meta.description}
                </span>
              </div>
            </div>
          </button>
        )
      })}
    </div>
  )
}
