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
  // nginx instead. /api is proxied to the Go API at API_PROXY_TARGET (default
  // http://localhost:8080, the port run-local.sh uses). Not VITE_API_URL: that one is the
  // app's own API base (src/api/index.js, including /api/v1), so using it for both broke one.
  server: {
    port: 3000,
    proxy: {
      // '/api/' with the slash, so the /api-keys page is not sent to the API.
      '/api/': {
        target: process.env.API_PROXY_TARGET || 'http://localhost:8080',
        changeOrigin: true,
      }
    }
  }
})
