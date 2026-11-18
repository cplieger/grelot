import { el } from "@cplieger/reactive/dom";
import { enhanceRelativeTime } from "@cplieger/ui/relative-time";

import { icon } from "../icons.js";
import type { ApiResult } from "../transport.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

/**
 * When sending f's request again unchanged can succeed, which is when a
 * Retry may show: 0 after no answer or a 503, a 429's named wait, or null
 * when only a changed request can.
 */
export function retryableIn(f: Failure): number | null {
  if (f.code === "aborted") {
    return null;
  }
  if (f.status === 0 || f.status === 503) {
    return 0;
  }
  return f.status === 429 ? (f.retryAfterMs ?? 0) : null;
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
