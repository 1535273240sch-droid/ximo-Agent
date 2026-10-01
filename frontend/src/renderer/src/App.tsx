import { useStore } from './store/app-store'
import { useMemoryStore } from './store/memory-store'
import { useBackendBridge } from './hooks/useBackendBridge'
import { TitleBar } from './components/shell/TitleBar'
import { Sidebar } from './components/shell/Sidebar'
import { StatusBar } from './components/shell/StatusBar'
import { ChatView } from './components/chat/ChatView'
import { ExpertsView } from './components/experts/ExpertsView'
import { KnowledgeView } from './components/knowledge/KnowledgeView'
import { SettingsView } from './components/settings/SettingsView'
import { MemoryView } from './components/memory/MemoryView'

/**
 * app-store 的 view 联合类型加上本任务新增的 'memory'。
 *
 * 为什么不直接改 app-store：那个 store 由 Lead 持有，且 view 联合类型被多处既有
 * 代码依赖，动它会把改动面扩到本任务之外。setView 的实现是 `set({ view: v })`，
 * 对值本身没有任何校验，所以这里只在类型层面拓宽——运行时零风险，缺口集中在这一处。
 */
type AppView = 'chat' | 'experts' | 'knowledge' | 'memory' | 'settings'

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

  const view = useStore((s) => s.view) as AppView
  const sidebarOpen = useStore((s) => s.sidebarOpen)
  const setView = useStore((s) => s.setView)

  // Work Log 的「回忆 N 条记忆」条目点击后要跳到记忆页并聚焦该节点。
  // 用 hook 订阅 focusNode（而不是在回调里 getState）保证「切视图」与「聚焦」在
  // 同一次渲染里生效：否则记忆页挂载时 focusNodeId 还是空的，第一次聚焦会丢。
  const focusNode = useMemoryStore((s) => s.focusNode)

  /**
   * 记忆页联动入口：切到 memory 视图 + 写 memory-store 的 focusNodeId。
   *
   * 这里对 app-store 只做「读 view / 写 view」两件事，不新增任何字段：记忆页的
   * 状态全部住在 memory-store 里。
   */
  const openMemoryNode = (id: string): void => {
    ;(setView as unknown as (v: AppView) => void)('memory')
    focusNode(id)
  }

  return (
    <div className="flex h-full w-full flex-col overflow-hidden">
      <TitleBar />

      <div className="flex min-h-0 flex-1">
        {sidebarOpen && <Sidebar />}

        <main className="min-w-0 flex-1 overflow-hidden">
          {view === 'chat' && <ChatView onOpenMemoryNode={openMemoryNode} />}
          {view === 'experts' && <ExpertsView />}
          {view === 'knowledge' && <KnowledgeView />}
          {view === 'memory' && <MemoryView />}
          {view === 'settings' && <SettingsView />}
        </main>
      </div>

      <StatusBar />
    </div>
  )
}
