import React from 'react'
import ReactDOM from 'react-dom/client'
import './styles/global.css'
import { App } from './App'
import { applyTheme } from './themes/tokens'
import { useStore } from './store/app-store'

// 首帧之前先把主题令牌写进根节点，避免"先闪一下默认主题再切换"。
applyTheme(useStore.getState().theme)

const root = document.getElementById('root')
if (!root) {
  throw new Error('#root not found in index.html')
}

ReactDOM.createRoot(root).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>
)
