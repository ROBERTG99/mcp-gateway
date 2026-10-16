import { existsSync } from "node:fs";
import { defineConfig, devices } from "@playwright/test";

// In Claude Code on the web Chromium is preinstalled under /opt/pw-browsers (docs/DEVELOPMENT.md);
// elsewhere `pnpm exec playwright install chromium` provides the bundled one.
const cloudChromium = "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
const executablePath =
  process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE ?? (existsSync(cloudChromium) ? cloudChromium : undefined);

export default defineConfig({
  testDir: "./tests",
  timeout: 60_000,
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: process.env.CONSOLE_URL ?? "http://localhost:8200",
    trace: "retain-on-failure",
    ...devices["Desktop Chrome"],
    launchOptions: { executablePath },
  },
});
