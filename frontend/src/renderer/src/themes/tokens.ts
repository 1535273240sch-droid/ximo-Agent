/**
 * 极简奢华视觉系统设计规范 (Minimalist Premium Design System).
 *
 * 核心哲学：
 *   MINIMAL · QUIET · PRECISE · PREMIUM · TIMELESS · INTELLIGENT
 *
 * 视觉准则：
 *   - 绝无朋克、绝无霓虹、绝无炫光、绝无过度毛玻璃雾化
 *   - 绝不使用 backdrop-filter 污染正文容器（避免文字模糊/发虚）
 *   - 像素级清晰：主文本权重 500–600 (不透明度 1.0)，暗色近白、亮色近黑
 *   - 完整支持 CJK / 汉字排版：优先 Inter / Geist / SF Pro + Noto Sans SC / 苹方
 *   - 最小字体尺寸 12px，常用正文 13–14px，标题 20–28px
 *   - 边界线极淡：仅以 3–5% 不透明度的高光细线勾勒轮廓，绝不喧宾夺主
 *   - 8–10px 内敛圆角，近乎无感的自然漫射投影
 *
 * 五套高定主题：
 *   1. Obsidian (黑曜石)   —— 深邃纯黑高级感 (#09090B)
 *   2. Graphite (石墨)     —— 柔和暗色开发者工作站 (#111214)
 *   3. Silver (银白)       —— 建筑学极简纯净亮色 (#F5F5F3)
 *   4. Warm White (暖白)   —— 奢华编辑部典雅米白 (#F3F1EC)
 *   5. Paper (原纸)        —— 极简设计工具温暖纸质感 (#EEECE6)
 */

export type ThemeId = 'obsidian' | 'graphite' | 'silver' | 'warm-white' | 'paper'

export interface ThemeTokens {
  id: ThemeId
  name: string
  isDark: boolean

  // 基础材质层
  canvas: string          // 画布底层
  surface: string         // 基础面板层
  surfaceRaised: string   // 悬浮/激活/弹出层
  border: string          // 边界线基色
  /** 边界线不透明度。刻意压得很低（3–5%），让它只作为极淡的轮廓提示。 */
  borderAlpha: number

  // 文字层次 (严格保证极端对比度与清晰度)
  ink: string             // Primary: 权重 500-600, 纯正高对比度
  inkMuted: string        // Secondary: 权重 400-450, 辅助文字
  inkFaint: string        // Muted/Tertiary: 弱提示、时间戳、快捷键

  // 强调与状态
  accent: string
  accentInk: string
  danger: string
  success: string
  warning: string

  // 字体栈 (排版引擎)
  fontUi: string
  fontDisplay: string
  fontMono: string

  // 几何形态与投影
  radius: string
  shadowSubtle: string
  shadowCard: string
}

// 统一的西文 + CJK 字体栈，确保跨平台在标准屏与 Retina 屏上都呈现像素级刀锋锐利度
const FONT_UI =
  "'Inter', 'Geist', -apple-system, BlinkMacSystemFont, 'SF Pro Text', 'PingFang SC', 'Hiragino Sans GB', 'Noto Sans SC', 'Noto Sans CJK SC', 'Microsoft YaHei', sans-serif"
const FONT_DISPLAY =
  "'Inter', 'Geist', -apple-system, BlinkMacSystemFont, 'SF Pro Display', 'PingFang SC', 'Hiragino Sans GB', 'Noto Sans SC', 'Noto Sans CJK SC', 'Microsoft YaHei', sans-serif"
const FONT_MONO =
  "'JetBrains Mono', 'Geist Mono', 'SF Mono', Consolas, 'Noto Sans Mono CJK SC', monospace"

/**
 * THEME 01 — OBSIDIAN (黑曜石)
 * 极简纯黑奢华感，专注、克制、技术美学。
 */
const obsidian: ThemeTokens = {
  id: 'obsidian',
  name: '黑曜石 (Obsidian)',
  isDark: true,
  canvas: '9 9 11',          // #09090B
  surface: '16 16 18',       // #101012
  surfaceRaised: '21 21 23', // #151517
  border: '255 255 255',
  borderAlpha: 0.038,
  ink: '245 245 245',        // #F5F5F5
  inkMuted: '146 146 153',   // #929299
  inkFaint: '95 96 103',     // #5F6067
  accent: '99 102 241',      // 高阶靛蓝
  accentInk: '255 255 255',
  danger: '239 68 68',
  success: '16 185 129',
  warning: '245 158 11',
  fontUi: FONT_UI,
  fontDisplay: FONT_DISPLAY,
  fontMono: FONT_MONO,
  radius: '10px',
  shadowSubtle: '0 1px 2px 0 rgba(0, 0, 0, 0.35)',
  shadowCard: '0 4px 16px -2px rgba(0, 0, 0, 0.45), 0 1px 2px 0 rgba(0, 0, 0, 0.25)'
}

/**
 * THEME 02 — GRAPHITE (石墨)
 * 柔和暗色，顶级专业开发者工作站质感。
 */
const graphite: ThemeTokens = {
  id: 'graphite',
  name: '石墨 (Graphite)',
  isDark: true,
  canvas: '17 18 20',        // #111214
  surface: '23 25 28',       // #17191C
  surfaceRaised: '29 31 34', // #1D1F22
  border: '255 255 255',
  borderAlpha: 0.042,
  ink: '241 241 240',        // #F1F1F0
  inkMuted: '155 156 159',   // #9B9C9F
  inkFaint: '101 103 107',   // #65676B
  accent: '56 189 248',      // 冰极冷天蓝
  accentInk: '15 23 42',
  danger: '248 113 113',
  success: '52 211 153',
  warning: '251 191 36',
  fontUi: FONT_UI,
  fontDisplay: FONT_DISPLAY,
  fontMono: FONT_MONO,
  radius: '10px',
  shadowSubtle: '0 1px 2px 0 rgba(0, 0, 0, 0.3)',
  shadowCard: '0 4px 16px -2px rgba(0, 0, 0, 0.4), 0 1px 2px 0 rgba(0, 0, 0, 0.2)'
}

/**
 * THEME 03 — SILVER (银白)
 * 建筑学般精确纯净的顶级亮色界面。
 */
const silver: ThemeTokens = {
  id: 'silver',
  name: '银白 (Silver)',
  isDark: false,
  canvas: '245 245 243',     // #F5F5F3
  surface: '255 255 255',    // #FFFFFF
  surfaceRaised: '250 250 249',
  border: '0 0 0',
  borderAlpha: 0.05,
  ink: '24 24 26',           // #18181A
  inkMuted: '102 103 107',   // #66676B
  inkFaint: '153 154 158',   // #999A9E
  accent: '37 99 235',       // 克制深蓝
  accentInk: '255 255 255',
  danger: '220 38 38',
  success: '5 150 105',
  warning: '217 119 6',
  fontUi: FONT_UI,
  fontDisplay: FONT_DISPLAY,
  fontMono: FONT_MONO,
  radius: '10px',
  shadowSubtle: '0 1px 2px 0 rgba(0, 0, 0, 0.03)',
  shadowCard: '0 4px 14px -2px rgba(0, 0, 0, 0.05), 0 1px 2px 0 rgba(0, 0, 0, 0.02)'
}

/**
 * THEME 04 — WARM WHITE (暖白)
 * 奢华出版物与典雅编辑部极简主义。
 */
const warmWhite: ThemeTokens = {
  id: 'warm-white',
  name: '暖白 (Warm White)',
  isDark: false,
  canvas: '243 241 236',     // #F3F1EC
  surface: '250 249 246',    // #FAF9F6
  surfaceRaised: '255 255 255',
  border: '40 35 30',
  borderAlpha: 0.055,
  ink: '32 32 30',           // #20201E
  inkMuted: '119 115 109',   // #77736D
  inkFaint: '160 155 147',   // #A09B93
  accent: '161 98 7',        // 古典古铜琥珀
  accentInk: '255 255 255',
  danger: '185 28 28',
  success: '21 128 61',
  warning: '180 83 9',
  fontUi: FONT_UI,
  fontDisplay: FONT_DISPLAY,
  fontMono: FONT_MONO,
  radius: '10px',
  shadowSubtle: '0 1px 2px 0 rgba(40, 35, 30, 0.04)',
  shadowCard: '0 4px 14px -2px rgba(40, 35, 30, 0.06), 0 1px 2px 0 rgba(40, 35, 30, 0.03)'
}

/**
 * THEME 05 — PAPER (原纸)
 * 极致纯粹的设计工具质感。
 */
const paper: ThemeTokens = {
  id: 'paper',
  name: '原纸 (Paper)',
  isDark: false,
  canvas: '238 236 230',     // #EEECE6
  surface: '247 246 241',    // #F7F6F1
  surfaceRaised: '252 251 248',
  border: '32 32 29',
  borderAlpha: 0.048,
  ink: '32 32 29',           // #20201D
  inkMuted: '116 113 106',   // #74716A
  inkFaint: '156 152 143',   // #9C988F
  accent: '45 42 38',        // 高贵深碳色
  accentInk: '247 246 241',
  danger: '153 27 27',
  success: '22 101 52',
  warning: '146 64 14',
  fontUi: FONT_UI,
  fontDisplay: FONT_DISPLAY,
  fontMono: FONT_MONO,
  radius: '9px',
  shadowSubtle: '0 1px 2px 0 rgba(32, 32, 29, 0.03)',
  shadowCard: '0 3px 12px -2px rgba(32, 32, 29, 0.05), 0 1px 2px 0 rgba(32, 32, 29, 0.02)'
}

export const THEMES: Record<ThemeId, ThemeTokens> = {
  obsidian,
  graphite,
  silver,
  'warm-white': warmWhite,
  paper
}

export const DEFAULT_THEME: ThemeId = 'obsidian'

export interface ThemeMeta {
  id: ThemeId
  name: string
  subtitle: string
  description: string
  swatch: [string, string, string]
  isDark: boolean
}

export const THEME_META: ThemeMeta[] = [
  {
    id: 'obsidian',
    name: '黑曜石',
    subtitle: 'Obsidian',
    description: '纯粹深黑极简奢华，微澜静谧，极致技术专注感',
    swatch: ['#09090B', '#101012', '#6366F1'],
    isDark: true
  },
  {
    id: 'graphite',
    name: '石墨',
    subtitle: 'Graphite',
    description: '柔和深灰，冷色调极客工作站，沉静而理性的工作氛围',
    swatch: ['#111214', '#17191C', '#38BDF8'],
    isDark: true
  },
  {
    id: 'silver',
    name: '银白',
    subtitle: 'Silver',
    description: '建筑学洁净极简，纯白与微冷灰交织，线条利落分明',
    swatch: ['#F5F5F3', '#FFFFFF', '#2563EB'],
    isDark: false
  },
  {
    id: 'warm-white',
    name: '暖白',
    subtitle: 'Warm White',
    description: '高级编辑部刊物风格，温润米白，典雅经久沉淀',
    swatch: ['#F3F1EC', '#FAF9F6', '#B45309'],
    isDark: false
  },
  {
    id: 'paper',
    name: '原纸',
    subtitle: 'Paper',
    description: '高定特种纸质感，自然内敛暖灰，如置身当代设计工作室',
    swatch: ['#EEECE6', '#F7F6F1', '#2D2A26'],
    isDark: false
  }
]

/**
 * 注入主题 CSS 变量至根 DOM 节点。
 */
export function applyTheme(id: ThemeId): void {
  const t = THEMES[id] || THEMES[DEFAULT_THEME]
  const root = document.documentElement

  root.style.setProperty('--c-canvas', t.canvas)
  root.style.setProperty('--c-surface', t.surface)
  root.style.setProperty('--c-surface-raised', t.surfaceRaised)
  root.style.setProperty('--c-border', t.border)
  root.style.setProperty('--c-border-alpha', String(t.borderAlpha))

  root.style.setProperty('--c-ink', t.ink)
  root.style.setProperty('--c-ink-muted', t.inkMuted)
  root.style.setProperty('--c-ink-faint', t.inkFaint)

  root.style.setProperty('--c-accent', t.accent)
  root.style.setProperty('--c-accent-ink', t.accentInk)
  root.style.setProperty('--c-danger', t.danger)
  root.style.setProperty('--c-success', t.success)
  root.style.setProperty('--c-warning', t.warning)

  root.style.setProperty('--font-ui', t.fontUi)
  root.style.setProperty('--font-display', t.fontDisplay)
  root.style.setProperty('--font-mono', t.fontMono)

  root.style.setProperty('--radius-main', t.radius)
  root.style.setProperty('--shadow-subtle', t.shadowSubtle)
  root.style.setProperty('--shadow-card', t.shadowCard)

  root.setAttribute('data-theme', t.id)
  root.setAttribute('data-color-scheme', t.isDark ? 'dark' : 'light')
}
