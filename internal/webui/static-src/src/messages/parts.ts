import { el } from "@cplieger/reactive/dom";
import { enhanceRelativeTime } from "@cplieger/ui/relative-time";

import { icon } from "../icons.js";
import type { ApiResult } from "../transport.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

// The statuses after which the same request sent again can succeed: no
// answer, a proxy that could not reach or wait for the server, a server not
// serving yet, and a rate limit.
const transient = new Set([0, 429, 502, 503, 504]);

/**
 * When sending f's request again unchanged can succeed, which is when a
 * Retry may show: its Retry-After wait (0 when it names none) after a
 * transient status, or null when only a changed request can. The status
 * decides, never the body.
 */
export function retryableIn(f: Failure): number | null {
  if (f.code === "aborted" || !transient.has(f.status)) {
    return null;
  }
  return f.retryAfterMs ?? 0;
}

/** Runs show once retryableIn(f) has passed, unless signal ends first; never when it is null. */
export function whenRetryable(f: Failure, signal: AbortSignal, show: () => void): void {
  const ms = retryableIn(f);
  if (ms === null || signal.aborted) {
    return;
  }
  if (ms === 0) {
    show();
    return;
  }
  const timer = setTimeout(show, ms);
  signal.addEventListener("abort", () => {
    clearTimeout(timer);
  });
}

/** A "Retry" button that runs run. */
export function retryButton(run: () => void): HTMLElement {
  const retry = el("button", { className: "cp-button", type: "button" }, "Retry");
  retry.addEventListener("click", run);
  return retry;
}

/** A button enhanceConfirm can mark busy: its spinner part, then its words. */
export function busyButton(words: string): HTMLButtonElement {
  const busy = icon("spinner-arc");
  busy.setAttribute("data-part", "busy");
  return el(
    "button",
    { className: "cp-button", type: "button" },
    busy,
    el("span", { className: "cp-roll" }, words),
  ) as HTMLButtonElement;
}

/** A <time> of at, in words kept current until signal aborts, the exact time on hover. */
export function relativeTime(at: number, signal: AbortSignal): HTMLTimeElement {
  const time = el("time", { datetime: new Date(at).toISOString() }) as HTMLTimeElement;
  enhanceRelativeTime(time, { signal });
  return time;
}
