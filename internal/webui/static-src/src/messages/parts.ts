import { el } from "@cplieger/reactive/dom";
import { enhanceRelativeTime } from "@cplieger/ui/relative-time";

import { icon } from "../icons.js";

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
