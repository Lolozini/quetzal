import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Dev server proxies the API (and console WebSocket) to the Go apiserver.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": {
        target: "http://localhost:8080",
        changeOrigin: true,
        ws: true,
      },
    },
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
  },
});
