import { defineConfig } from "vite";
import solid from "vite-plugin-solid";
import tailwind from "@tailwindcss/vite";

// Build output goes into the Go embed package (internal/webui/dist).
// Relative base so the single binary can serve from any mount point.
export default defineConfig({
  plugins: [solid(), tailwind()],
  base: "./",
  build: {
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    // dev: proxy API calls to the running swarmd
    proxy: {
      "/api": "http://127.0.0.1:8888",
    },
  },
});
