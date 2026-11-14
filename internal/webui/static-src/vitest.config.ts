// A file runs in headless Chromium unless it is named `.node.test.ts`, which
// runs in Node. `channel: "chromium"` is the full browser, not headless-shell.
import { configDefaults, defineConfig } from "vitest/config";
import { playwright } from "@vitest/browser-playwright";
import { msw } from "msw/vite";

export default defineConfig({
  // Serves the MSW worker script for the browser tests that fake the
  // server; each such test starts its own worker.
  plugins: [msw({ mode: "worker-only" })],
  // cmd/bundle defines the stamped icon folder; a test serves no icons.
  define: { __ICON_BASE__: JSON.stringify("/icons/") },
  test: {
    projects: [
      {
        // `extends` is a project key: inside `test` it is ignored and the
        // project silently loses every root option.
        extends: true,
        test: {
          name: "node",
          environment: "node",
          pool: "threads",
          isolate: true,
          include: ["src/**/*.node.test.ts"],
        },
      },
      {
        extends: true,
        test: {
          name: "browser",
          include: ["src/**/*.test.ts"],
          exclude: [...configDefaults.exclude, "src/**/*.node.test.ts"],
          browser: {
            enabled: true,
            headless: true,
            provider: playwright({
              launchOptions: {
                channel: "chromium",
                // Chromium throttles animation frames after about 49 files in
                // one session without it.
                args: ["--disable-frame-rate-limit"],
              },
            }),
            instances: [{ browser: "chromium" }],
            viewport: { width: 1280, height: 720 },
            screenshotFailures: false,
          },
        },
      },
    ],
    passWithNoTests: false,
    allowOnly: false,
    globals: false,
    expect: { requireAssertions: true },
    clearMocks: true,
    mockReset: true,
    restoreMocks: true,
    unstubEnvs: true,
    unstubGlobals: true,
    retry: process.env["CI"] ? 1 : 0,
    sequence: { shuffle: { files: false, tests: false }, hooks: "stack" },
  },
});
