import { onTestFinished } from "vitest";

/**
 * Settles with check's value once it stops throwing, re-run every 10 ms.
 * It has no deadline of its own, unlike vi.waitFor's 1 s, which host load
 * outlasts; a check that never passes fails at the test timeout with the
 * check's last error.
 */
export async function until<T>(check: () => T): Promise<T> {
  let last: unknown = new Error("the check never ran");
  let running = true;
  onTestFinished(() => {
    if (running) {
      running = false;
      throw last;
    }
  });
  while (running) {
    try {
      const value = check();
      running = false;
      return value;
    } catch (error) {
      last = error;
      await new Promise((settle) => setTimeout(settle, 10));
    }
  }
  throw last;
}
