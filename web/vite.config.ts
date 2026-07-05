// Vite configuration for the console dev server and API proxy.

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Export one shared Vite config for local dev, production builds, and Playwright startup.
export default defineConfig({
  // React plugin enables TSX transformation and Fast Refresh during local console development.
  plugins: [react()],
  server: {
    // Keep this aligned with the Playwright runner default port.
    port: 5173,
    proxy: {
      "/api": {
        // Tests point this proxy at the mock API; dev mode falls back to the local web server.
        target: process.env.LOGSERVE_WEB_API_PROXY ?? "http://127.0.0.1:8080",
        // Rewrite the origin so the mock API and local Go server see a direct gateway-style request.
        changeOrigin: true
      }
    }
  }
});
