import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// En dev (`npm run dev`) el frontend corre en :5173 y proxea al backend Go.
// En producción el propio backend sirve `dist/`, así que no hay proxy.
const backend = process.env.WEBTERM_BACKEND ?? 'http://127.0.0.1:7788'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: backend, changeOrigin: true },
      '/ws': { target: backend, ws: true, changeOrigin: true },
    },
  },
})
