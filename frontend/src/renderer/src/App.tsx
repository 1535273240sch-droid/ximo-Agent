import { useStore } from './store/app-store'
import { useBackendBridge } from './hooks/useBackendBridge'
import { TitleBar } from './components/shell/TitleBar'
import { Sidebar } from './components/shell/Sidebar'
import { StatusBar } from './components/shell/StatusBar'
import { ChatView } from './components/chat/ChatView'
import { ExpertsView } from './components/experts/ExpertsView'
import { KnowledgeView } from './components/knowledge/KnowledgeView'
import { SettingsView } from './components/settings/SettingsView'

/**
 * 应用外壳。
 *
 * 结构（自外向内）：
 *   ┌ TitleBar（无边框窗口的自绘标题栏，可拖拽）──────────┐
 *   │ Sidebar │ 主视图区（按 view 切换）                  │
 *   └ StatusBar（后端连接状态、当前 run）─────────────────┘
 *
 * 玻璃质感由各层的 .glass-* 工具类叠加实现：背景装饰层在最底，侧栏与主面板
 * 是不同透明度的玻璃，浮层再叠一层更亮的玻璃。
 */
export function App(): React.JSX.Element {
  useBackendBridge()

  const view = useStore((s) => s.view)
  const sidebarOpen = useStore((s) => s.sidebarOpen)

  return (
    <div className="flex h-full w-full flex-col overflow-hidden">
      <TitleBar />

      <div className="flex min-h-0 flex-1">
        {sidebarOpen && <Sidebar />}

        <main className="min-w-0 flex-1 overflow-hidden">
          {view === 'chat' && <ChatView />}
          {view === 'experts' && <ExpertsView />}
          {view === 'knowledge' && <KnowledgeView />}
          {view === 'settings' && <SettingsView />}
        </main>
      </div>

      <StatusBar />
    </div>
  )
}
