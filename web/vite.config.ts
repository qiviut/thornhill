import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// Dev proxy: the Go gateway runs on :8787; Vite serves the UI with HMR.
const target = "http://localhost:8787";

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/offer": target,
      "/events": target,
      "/audio": target,
      "/hooks": target,
      "/ws": { target, ws: true },
    },
  },
  build: { outDir: "dist" },
});
