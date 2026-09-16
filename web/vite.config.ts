import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";
import { writeFileSync } from "node:fs";

// The gateway embeds this bundle with `//go:embed all:dist`. A committed
// dist/.gitkeep keeps that glob satisfied on a fresh checkout, so `go build`
// works before Bun has ever run. Vite empties the directory at build start,
// so the placeholder is written back afterwards.
function keepDistGitkeep() {
  return {
    name: "keep-dist-gitkeep",
    closeBundle() {
      writeFileSync(path.resolve(import.meta.dirname, "dist/.gitkeep"), "");
    },
  };
}

export default defineConfig({
  plugins: [react(), tailwindcss(), keepDistGitkeep()],
  resolve: { alias: { "@": path.resolve(import.meta.dirname, "./src") } },
  server: {
    port: 5173,
    // The dev server proxies to a local gateway, so the session cookie is
    // same-origin in development too.
    proxy: {
      "/v1": { target: "http://localhost:8080", ws: true },
      "/auth": "http://localhost:8080",
      "/operator": "http://localhost:8080",
    },
  },
});
