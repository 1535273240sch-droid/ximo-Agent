import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import { fileURLToPath } from 'node:url'

const resolveFromRoot = (p: string): string => fileURLToPath(new URL(p, import.meta.url))

/**
 * 最小渲染层单测配置（P0-a 引入）。
 *
 * 只用于 renderer 侧的组件单测：jsdom 环境 + 与 electron.vite.config.ts 相同的
 * @ / @shared 别名。不参与 electron-vite 的 main/preload/renderer 构建。
 */
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': resolveFromRoot('./src/renderer/src'),
      '@shared': resolveFromRoot('./src/shared')
    }
  },
  test: {
    environment: 'jsdom',
    include: ['src/renderer/src/**/*.test.{ts,tsx}'],
    globals: false,
    restoreMocks: true
  }
})
