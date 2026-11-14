import { el } from "@cplieger/reactive/dom";
import { anchorTo } from "@cplieger/ui/anchor";
import { enhancePanel } from "@cplieger/ui/panel";

import { icon } from "../icons.js";
import { getReceipts } from "../wire/client.gen.js";
import type { ReadCount, Receipt, Receipts } from "../wire/types.gen.js";
import { agentLink, senderWords } from "./names.js";

interface ReadSummary {
  /** The "Read by 3 of 5" button and the panel it opens, for an entry's footer. */
  readonly nodes: readonly HTMLElement[];
  update(count: ReadCount): void;
}

function readWords(count: ReadCount): string {
  return `Read by ${String(count.read)} of ${String(count.total)}`;
}

function inlineLoad(words: string): HTMLElement {
  return el(
    "span",
    { className: "cp-inline-load" },
    el("span", { className: "cp-loader", "aria-hidden": "true" }, el("span")),
    words,
  );
}

/** A recipient by name: a retired one has no agent page left, and its name may be another's now. */
function recipient(r: Receipt): HTMLElement {
  switch (r.state) {
    case "retired":
      return el("li", null, el("bdi", null, senderWords(r.name)), ", retired");
    case "dormant":
      return el("li", null, agentLink(r.name), ", dormant");
    default:
      return el("li", null, agentLink(r.name));
  }
}

function group(title: string, rs: readonly Receipt[], none: string): HTMLElement {
  return el(
    "section",
    null,
    el("h4", null, title),
    rs.length === 0
      ? el("p", { className: "cp-meta" }, none)
      : el("ul", null, ...rs.map(recipient)),
  );
}

function receiptsBody(rc: Receipts): HTMLElement[] {
  const read = rc.recipients.filter((r) => r.state === "read");
  const waiting = rc.recipients.filter((r) => r.state !== "read");
  const out = [
    group("Read", read, "No one yet."),
    group("Not read yet", waiting, "Everyone has read it."),
  ];
  if (rc.registered_since > 0) {
    const n = rc.registered_since;
    out.push(
      el(
        "p",
        { className: "cp-meta" },
        n === 1
          ? "1 agent registered after it and did not receive it."
          : `${String(n)} agents registered after it and did not receive it.`,
      ),
    );
  }
  return out;
}

/**
 * A message's read summary (feed.md "The parts"): who has read it, loaded
 * each time its panel opens, since receipts move while the page is open.
 */
export function readSummary(
  message: number,
  count: ReadCount,
  idPrefix: string,
  signal: AbortSignal,
): ReadSummary {
  const id = `${idPrefix}-readers-${String(message)}`;
  const button = el(
    "button",
    {
      className: "cp-button",
      "data-variant": "quiet",
      "data-size": "sm",
      type: "button",
      popovertarget: id,
    },
    readWords(count),
  );
  const panel = el("div", {
    className: "cp-popover cp-feed-readers",
    "data-variant": "panel",
    popover: "auto",
    id,
    "aria-label": "Who read this message",
  });
  anchorTo(panel, button);
  enhancePanel(panel, { signal });

  let loading: AbortController | null = null;
  let loaded = false;

  /** Focus that the panel's new content took the place of goes back into it. */
  const regains = (): boolean =>
    panel.matches(":popover-open") &&
    (panel.contains(document.activeElement) ||
      document.activeElement === button ||
      document.activeElement === document.body);

  const load = async (): Promise<void> => {
    loading?.abort();
    const stop = new AbortController();
    loading = stop;
    if (!loaded) {
      panel.setAttribute("aria-busy", "true");
      panel.replaceChildren(inlineLoad("Loading who read it…"));
    }
    const result = await getReceipts(
      { id: message },
      { signal: AbortSignal.any([signal, stop.signal]) },
    );
    if (stop.signal.aborted || signal.aborted) {
      return;
    }
    loading = null;
    panel.removeAttribute("aria-busy");
    const regain = regains();
    if (!result.ok) {
      loaded = false;
      const retry = el(
        "button",
        { className: "cp-button", "data-size": "sm", type: "button" },
        "Retry",
      );
      retry.addEventListener("click", () => {
        void load();
      });
      panel.replaceChildren(
        el(
          "div",
          { className: "cp-notice", "data-tone": "danger" },
          icon("circle-x"),
          el(
            "div",
            { "data-part": "body" },
            `Who read it could not be loaded. ${result.message ?? "grelot did not answer."} `,
            retry,
          ),
        ),
      );
      if (regain) {
        retry.focus();
      }
      return;
    }
    loaded = true;
    panel.replaceChildren(...receiptsBody(result.data));
    const heading = panel.querySelector("h4");
    if (regain && heading !== null) {
      heading.tabIndex = -1;
      heading.focus();
    }
  };

  panel.addEventListener(
    "toggle",
    (e) => {
      if (e.newState === "open") {
        void load();
      } else {
        loading?.abort();
      }
    },
    { signal },
  );

  return {
    nodes: [button, panel],
    update(next) {
      const words = readWords(next);
      if (button.textContent !== words) {
        button.textContent = words;
      }
    },
  };
}
