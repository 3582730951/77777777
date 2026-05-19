import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'

export default defineConfig({
  base: process.env.VITE_BASE || '/autoreg/',
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  build: {
    outDir: process.env.VITE_OUT_DIR || '../../../internal/admin/autoreg_spa',
    emptyOutDir: true,
  },
  server: {
    proxy: {
      '/api': 'http://localhost:9900',
    },
  },
})
