// Vite configuration for the console dev server and API proxy.

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/api": {
        // Tests point this proxy at the mock API; dev mode falls back to the local web server.
        target: process.env.LOGSERVE_WEB_API_PROXY ?? "http://127.0.0.1:8080",
        changeOrigin: true
      }
    }
  }
});
