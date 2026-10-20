import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  test: {
    environment: "jsdom",
    setupFiles: ["./src/testSetup.ts"],
    environmentOptions: {
      jsdom: { url: "http://127.0.0.1:5173/" },
    },
    restoreMocks: true,
  },
});
