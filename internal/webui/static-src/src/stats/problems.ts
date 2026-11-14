import { el } from "@cplieger/reactive/dom";
import { closeDialog } from "@cplieger/ui/dialog";

import { icon } from "../icons.js";
import { agentLink } from "../messages/names.js";
import { relativeTime } from "../messages/parts.js";
import type { Route } from "../router.js";
import type { Event, EventKind, Stats } from "../wire/types.gen.js";
import { rangeWords } from "./charts.js";
import { figure, type Figure } from "./figures.js";

/** A Needs attention figure's dialog, as the address names it. */
export type Problems = NonNullable<Extract<Route, { readonly screen: "statistics" }>["problems"]>;

interface Kind {
  readonly label: string;
  readonly kinds: readonly EventKind[];
  readonly count: (s: Stats) => number;
  /** "No post was refused in the last 6 hours." */
  readonly none: (range: string) => string;
}

const problemKinds: Readonly<Record<Problems, Kind>> = {
  refused: {
    label: "Refused posts",
    kinds: ["post_refused"],
    count: (s) => s.event_counts.post_refused,
    none: (r) => `No post was refused in the last ${r}.`,
  },
  push: {
    label: "Failed pushes",
    kinds: ["push_retry", "push_gave_up"],
    count: (s) => s.event_counts.push_retry + s.event_counts.push_gave_up,
    none: (r) => `No push failed in the last ${r}.`,
  },
  nudges: {
    label: "Repeated nudges",
    kinds: ["nudge_repeat"],
    count: (s) => s.event_counts.nudge_repeat,
    none: (r) => `No agent was nudged twice in the last ${r}.`,
  },
};

const order: readonly Problems[] = ["refused", "push", "nudges"];

/** The words for a refusal's reason (internal/obs Refused*), or null for one this build does not know. */
function refusalWords(reason: string): string | null {
  switch (reason) {
    case "store_full":
      return "The store was full.";
    case "rate_limited":
      return "The sender posted too often.";
    case "scope":
      return "The sender may not post there.";
    default:
      return null;
  }
}

/** The words for a nudge's layer (internal/store NudgeVia), or null for one this build does not know. */
function layerWords(via: string): string | null {
  switch (via) {
    case "push":
      return "Nudged again by push.";
    case "watcher":
      return "Nudged again by its watcher.";
    case "hook":
      return "Nudged again by a hook.";
    default:
      return null;
  }
}

interface StateWords {
  readonly tone: "warning" | "danger";
  readonly glyph: "circle-alert" | "circle-x";
  readonly words: string;
}

function state(kind: EventKind): StateWords {
  switch (kind) {
    case "post_refused":
      return { tone: "warning", glyph: "circle-alert", words: "Refused" };
    case "push_retry":
      return { tone: "warning", glyph: "circle-alert", words: "Retried" };
    case "push_gave_up":
      return { tone: "danger", glyph: "circle-x", words: "Gave up" };
    case "nudge_repeat":
      return { tone: "warning", glyph: "circle-alert", words: "Nudged again" };
  }
}

/** The event's reason: the server's code in words, or the code itself in mono when unknown. */
function reason(e: Event): Node {
  switch (e.kind) {
    case "post_refused":
    case "nudge_repeat": {
      const words = e.kind === "post_refused" ? refusalWords(e.detail) : layerWords(e.detail);
      return words === null
        ? el("code", { className: "cp-mono" }, e.detail)
        : document.createTextNode(words);
    }
    case "push_retry":
    case "push_gave_up":
      return el(
        "span",
        null,
        "Through ",
        el("code", { className: "cp-mono", dir: "ltr" }, e.detail),
      );
  }
}

/** A problem row (list-row.md "What each case writes"): the agent and the reason, its time, its state. */
function eventRow(e: Event, signal: AbortSignal): HTMLElement {
  const { tone, glyph, words } = state(e.kind);
  const lead = icon(glyph);
  lead.dataset["tone"] = tone;
  return el(
    "li",
    null,
    el(
      "div",
      { className: "cp-list-row", "data-lines": "wrap" },
      lead,
      el(
        "div",
        null,
        el("p", null, agentLink(e.agent), ": ", reason(e)),
        relativeTime(e.at_ms, signal),
      ),
      el(
        "span",
        { "data-part": "end" },
        el("span", { className: "cp-state", "data-tone": tone }, words),
      ),
    ),
  );
}

export interface NeedsAttention {
  readonly element: HTMLElement;
  /** The three dialogs, appended once beside the section. */
  readonly dialogs: HTMLElement;
  draw(s: Stats, range: keyof typeof rangeWords): void;
  /** Opens the problems dialog the address names, or closes the open one for null. */
  show(which: Problems | null): void;
}

/**
 * The Needs attention figures and the dialog each opens with its recent
 * events. A dialog's opening is in the address: a figure asks onOpen, and
 * closing it asks onClose.
 */
export function needsAttention(opts: {
  readonly signal: AbortSignal;
  readonly onOpen: (which: Problems) => void;
  readonly onClose: () => void;
}): NeedsAttention {
  const { signal } = opts;
  const parts = order.map((which) => {
    const kind = problemKinds[which];
    const id = `stats-problems-${which}`;
    const f: Figure = figure(kind.label, { opens: id });
    f.element.addEventListener(
      "click",
      () => {
        opts.onOpen(which);
      },
      { signal },
    );
    // Focusable, so showModal puts focus on it: the dialog holds content to read.
    const heading = el("h2", { id: `${id}-title`, tabIndex: -1 }, kind.label);
    const body = el("div", { className: "cp-dialog-body" });
    const close = el("button", { className: "cp-button", type: "button" }, "Close");
    const dialog = el(
      "dialog",
      { className: "cp-dialog", id, "aria-labelledby": heading.id },
      el("header", null, heading),
      body,
      el("footer", null, close),
    ) as HTMLDialogElement;
    close.addEventListener(
      "click",
      () => {
        closeDialog(dialog, "close");
      },
      { signal },
    );
    dialog.addEventListener(
      "close",
      () => {
        f.element.setAttribute("aria-expanded", "false");
        if (open === which) {
          open = null;
          opts.onClose();
        }
      },
      { signal },
    );
    return { which, kind, figure: f, dialog, body, key: "" };
  });
  let open: Problems | null = null;
  let last: Stats | null = null;
  let lastRange: keyof typeof rangeWords = "24h";

  const fill = (p: (typeof parts)[number]): void => {
    if (last === null) {
      return;
    }
    const events = last.events.filter((e) => p.kind.kinds.includes(e.kind));
    const key = JSON.stringify([events, lastRange]);
    if (key === p.key) {
      return;
    }
    p.key = key;
    const count = p.kind.count(last);
    p.body.replaceChildren(
      events.length === 0
        ? el("p", { className: "cp-meta" }, p.kind.none(rangeWords[lastRange]))
        : el(
            "ul",
            { className: "cp-list", "aria-label": p.kind.label },
            ...events.map((e) => eventRow(e, signal)),
          ),
      ...(count > events.length
        ? [
            el(
              "p",
              { className: "cp-meta" },
              `The newest ${String(events.length)} of ${String(count)}. The server keeps the newest 200 problems of all kinds together.`,
            ),
          ]
        : []),
    );
  };

  const present = (which: Problems | null): void => {
    for (const p of parts) {
      if (p.which === which) {
        fill(p);
        if (!p.dialog.open) {
          p.dialog.showModal();
          p.figure.element.setAttribute("aria-expanded", "true");
        }
      } else if (p.dialog.open) {
        closeDialog(p.dialog, "route");
      }
    }
  };

  return {
    element: el("div", { className: "cp-grid" }, ...parts.map((p) => p.figure.element)),
    dialogs: el("div", null, ...parts.map((p) => p.dialog)),
    draw(s, range) {
      last = s;
      lastRange = range;
      for (const p of parts) {
        const n = p.kind.count(s);
        p.figure.set(n, new Intl.NumberFormat().format(n), {
          tone: n === 0 ? "neutral" : "warning",
          words: "",
        });
        if (p.dialog.open) {
          fill(p);
        }
      }
      if (open !== null) {
        present(open);
      }
    },
    show(which) {
      open = which;
      if (last !== null) {
        present(which);
      }
    },
  };
}
