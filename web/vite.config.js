import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vite'

// 后端端口由 start.sh 通过 API_PORT 注入，未设置时回退 8080
const API_PORT = process.env.API_PORT || '8080'

// WSL 的 /mnt 挂载（drvfs）不支持 inotify，需轮询才能保证热更新生效。
// start.sh 在项目位于 /mnt 下时会自动置 VITE_POLL=1。
const usePolling = process.env.VITE_POLL === '1'

export default defineConfig({
  plugins: [vue()],
  server: {
    // 使用 127.0.0.1 而非 localhost，避免解析到 ::1 导致 WSL 下代理失败
    proxy: {
      '/api': {
        target: `http://127.0.0.1:${API_PORT}`,
        changeOrigin: true,
      },
    },
    ...(usePolling ? { watch: { usePolling: true, interval: 1000 } } : {}),
  },
})
