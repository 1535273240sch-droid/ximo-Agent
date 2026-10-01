/**
 * d3-force 的最小本地类型声明。
 *
 * 为什么自己声明而不是装 @types/d3-force：本任务的硬约束是「只允许新增一个依赖
 * d3-force」。@types/* 会进 devDependencies 并出现在 lockfile 里，那已经是第二个
 * 依赖了。这里只声明实际用到的 API（forceSimulation / forceLink / forceManyBody /
 * forceCenter / forceCollide 与 SimulationNodeDatum / SimulationLinkDatum），
 * 面窄且可审计。
 *
 * 声明文件放在 renderer 下是因为它只被图视图使用；tsconfig.web.json 的 include
 * 覆盖 `src/renderer/src/**`，因此会被自动纳入。
 */
declare module 'd3-force' {
  /** 参与力导向布局的节点。d3 会就地写入 x/y/vx/vy。 */
  export interface SimulationNodeDatum {
    index?: number
    x?: number
    y?: number
    vx?: number
    vy?: number
    /** 非 undefined 时该节点被钉住（拖拽中）。 */
    fx?: number | null
    fy?: number | null
  }

  /** 参与力导向布局的边。source/target 会被 d3 从 id 解析成节点对象。 */
  export interface SimulationLinkDatum<N> {
    source: string | N
    target: string | N
    index?: number
  }

  export interface Simulation<N> {
    /** 注册/替换一条力；name 为 'link' | 'charge' | 'center' | 'collide' 等。 */
    force(name: string, force?: unknown): Simulation<N>
    nodes(nodes: N[]): Simulation<N>
    // 读写同名的链式 API：重载顺序必须是「无参在前」，否则 sim.alpha() 会命中
    // 带参重载、返回类型退化成 number。
    alpha(): number
    alpha(alpha: number): Simulation<N>
    alphaMin(): number
    alphaMin(min: number): Simulation<N>
    alphaDecay(): number
    alphaDecay(decay: number): Simulation<N>
    alphaTarget(): number
    alphaTarget(target: number): Simulation<N>
    velocityDecay(): number
    velocityDecay(decay: number): Simulation<N>
    /** 手动推进一帧（与 tick 事件配合可用于完全自控的渲染循环）。 */
    tick(iterations?: number): Simulation<N>
    restart(): Simulation<N>
    stop(): Simulation<N>
    on(typenames: string, listener: ((this: Simulation<N>) => void) | null): Simulation<N>
    find(x: number, y: number, radius?: number): N | undefined
  }

  export interface ForceLink<N, L> {
    (): L[]
    (links: L[]): ForceLink<N, L>
    id(accessor: (node: N, i: number, nodes: N[]) => string | number): ForceLink<N, L>
    distance(distance: number | ((link: L, i: number, links: L[]) => number)): ForceLink<N, L>
    strength(strength: number | ((link: L, i: number, links: L[]) => number)): ForceLink<N, L>
    iterations(iterations: number): ForceLink<N, L>
  }

  export interface ForceManyBody<N> {
    strength(strength: number | ((node: N, i: number, nodes: N[]) => number)): ForceManyBody<N>
    distanceMin(distance: number): ForceManyBody<N>
    distanceMax(distance: number): ForceManyBody<N>
    theta(theta: number): ForceManyBody<N>
  }

  export interface ForceCenter<N> {
    x(): number
    x(x: number): ForceCenter<N>
    y(): number
    y(y: number): ForceCenter<N>
    strength(): number
    strength(strength: number): ForceCenter<N>
  }

  export interface ForceCollide<N> {
    radius(radius: number | ((node: N, i: number, nodes: N[]) => number)): ForceCollide<N>
    strength(strength: number): ForceCollide<N>
    iterations(iterations: number): ForceCollide<N>
  }

  export function forceSimulation<N extends SimulationNodeDatum>(
    nodes?: N[]
  ): Simulation<N>
  export function forceLink<N extends SimulationNodeDatum, L extends SimulationLinkDatum<N>>(
    links?: L[]
  ): ForceLink<N, L>
  export function forceManyBody<N extends SimulationNodeDatum>(): ForceManyBody<N>
  export function forceCenter<N extends SimulationNodeDatum>(
    x?: number,
    y?: number
  ): ForceCenter<N>
  export function forceCollide<N extends SimulationNodeDatum>(
    radius?: number | ((node: N, i: number, nodes: N[]) => number)
  ): ForceCollide<N>
}
