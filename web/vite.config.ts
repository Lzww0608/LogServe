import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        target: process.env.LOGSERVE_WEB_API_PROXY ?? "http://127.0.0.1:8080",
        changeOrigin: true
      }
    }
  }
});
