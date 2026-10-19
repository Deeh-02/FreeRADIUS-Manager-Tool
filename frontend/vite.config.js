import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath, URL } from 'node:url'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url))
    }
  },
  // Dev server only (npm run dev / run-local.sh). In Docker the built files are served by
  // nginx instead. /api is proxied to the Go API; VITE_API_URL here is read from the shell
  // at dev time and defaults to http://localhost:8080 (the port run-local.sh uses).
  server: {
    port: 3000,
    proxy: {
      '/api': {
        target: process.env.VITE_API_URL || 'http://localhost:8080',
        changeOrigin: true,
      }
    }
  }
})
