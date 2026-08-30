import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// Dev server proxies API calls to a running openvtld (set VTL_API to
// point at a development appliance for live-data development). The daemon serves HTTPS
// with a self-signed cert since v0.5 — secure:false trusts it.
const apiTarget = process.env.VTL_API ?? 'https://127.0.0.1:8443'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    // Preview harness assigns an alternate port via PORT when 5173 is taken.
    port: process.env.PORT ? Number(process.env.PORT) : 5173,
    proxy: {
      '/api': { target: apiTarget, changeOrigin: true, secure: false },
      '/metrics': { target: apiTarget, changeOrigin: true, secure: false },
    },
  },
})
