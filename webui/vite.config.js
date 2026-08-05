import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Built output lands in ../web, which is what the Go test console serves
// (WEB_DIR). `npm run dev` proxies API calls to a locally running console.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../web',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': 'http://127.0.0.1:8090',
    },
  },
  // The tests cover pure functions only, so they need no DOM.
  test: {
    environment: 'node',
    include: ['src/**/*.test.{js,jsx}'],
  },
})
