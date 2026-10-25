import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";
import { createViteInspector } from "../../packages/vite-inspector/index.mjs";

export default defineConfig({
  plugins: [
    createViteInspector({
      enabled: process.env.MCE4D_INSPECTOR === "1",
      mode: process.env.MCE4D_INSPECTOR_MODE,
      worktreeId: process.env.MCE4D_WORKTREE_ID,
      profileId: process.env.MCE4D_PROFILE_ID,
      outputDirectory: process.env.MCE4D_INSPECTOR_OUTPUT,
    }),
    react(),
  ],
  test: {
    environment: "jsdom",
    setupFiles: ["./src/testSetup.ts"],
    environmentOptions: {
      jsdom: { url: "http://127.0.0.1:5173/" },
    },
    restoreMocks: true,
  },
});
