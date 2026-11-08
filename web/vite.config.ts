import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// The UI is served by mapthing under /admin/ (the Go server falls back to index.html
// for unknown paths below /admin/). In dev, API calls are proxied to a local mapthing
// on :8080; `npm run dev:mock` needs no backend at all.
const backend = process.env.MAPTHING_URL ?? "http://localhost:8080";
const proxied = ["/admin/api", "/api", "/styles", "/tiles", "/packs", "/healthz", "/readyz"];

export default defineConfig(({ mode }) => ({
  base: "/admin/",
  plugins: [react(), tailwindcss()],
  define: {
    "import.meta.env.VITE_MOCK": JSON.stringify(mode === "mock" || mode === "test" ? "1" : ""),
  },
  server: {
    port: 5173,
    proxy:
      mode === "mock"
        ? undefined
        : Object.fromEntries(proxied.map((p) => [p, { target: backend, changeOrigin: false }])),
  },
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false, chunkSizeWarningLimit: 1500 },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["src/test/setup.ts"],
    css: false,
    // Generous per-test ceiling for a loaded machine (user-event typing is slow under load).
    testTimeout: 30_000,
  },
}));
