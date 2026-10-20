import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build goes to web/dist and is embedded into kubemap-portal (web/embed.go).
// `npm run dev` proxies the API to a portal running locally on :8080. It says
// "http" so the login cookie is not Secure (a browser drops that over plain http).
export default defineConfig({
  plugins: [react()],
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
  server: {
    proxy: {
      "/api": { target: "http://127.0.0.1:8080", headers: { "X-Forwarded-Proto": "http" } },
      "/v1": { target: "http://127.0.0.1:8080", headers: { "X-Forwarded-Proto": "http" } },
    },
  },
});
