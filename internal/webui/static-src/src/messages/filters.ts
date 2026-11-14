import { el } from "@cplieger/reactive/dom";

import { icon } from "../icons.js";
import type { ConversationCounts, Show } from "../wire/types.gen.js";

const choices: readonly { readonly show: Show; readonly word: string }[] = [
  { show: "all", word: "All" },
  { show: "threads", word: "Threads" },
  { show: "direct", word: "Direct" },
];

/** How many conversations show lists. */
export function countOf(show: Show, counts: ConversationCounts): number {
  switch (show) {
    case "all":
      return counts.threads + counts.direct;
    case "threads":
      return counts.threads;
    case "direct":
      return counts.direct;
  }
}

interface FilterGroup {
  readonly element: HTMLElement;
  /** Presses show's filter, and gives each its count once counts is known. */
  set(show: Show, counts: ConversationCounts | null): void;
}

/**
 * The "Show" filters, All, Threads and Direct; a press calls choose. In a
 * dialog, autofocus marks the pressed one, which the dialog then focuses.
 */
export function filterGroup(
  id: string,
  choose: (show: Show) => void,
  opts: { readonly autofocus?: boolean } = {},
): FilterGroup {
  const buttons = choices.map(({ show, word }) => {
    const button = el(
      "button",
      { className: "cp-list-row", type: "button", "aria-pressed": "false" },
      el("span", { className: "cp-cell-stack" }, word),
    );
    button.addEventListener("click", () => {
      choose(show);
    });
    return { show, word, button };
  });
  const element = el(
    "div",
    { role: "group", "aria-labelledby": `${id}-title` },
    el("div", { className: "cp-list-heading", id: `${id}-title` }, "Show"),
    el(
      "ul",
      {
        className: "cp-list",
        "data-surface": "plain",
        "data-divider": "none",
        "data-density": "compact",
      },
      ...buttons.map((b) => el("li", null, b.button)),
    ),
  );
  return {
    element,
    set(show, counts) {
      for (const b of buttons) {
        b.button.setAttribute("aria-pressed", String(b.show === show));
        if (opts.autofocus === true) {
          b.button.toggleAttribute("autofocus", b.show === show);
        }
        const n = counts === null ? null : countOf(b.show, counts);
        b.button.replaceChildren(
          el("span", { className: "cp-cell-stack" }, b.word),
          ...(n === null
            ? []
            : [
                el(
                  "span",
                  { "data-part": "end" },
                  el(
                    "span",
                    { className: "cp-badge", "data-variant": "quiet" },
                    String(n),
                    el(
                      "span",
                      { className: "cp-visually-hidden" },
                      n === 1 ? " conversation" : " conversations",
                    ),
                  ),
                ),
              ]),
        );
      }
    },
  };
}

interface FilterSheet {
  /** The Filters button, for the screen's toolbar. */
  readonly toggle: HTMLElement;
  readonly dialog: HTMLDialogElement;
  set(show: Show, counts: ConversationCounts | null): void;
}

/**
 * The filter sheet below 60rem (design-patterns.md "Dialogs and sheets"):
 * one fast group, so each press applies at once and Close keeps it. While a
 * filter is on, the toggle counts it and Clear filters sits beside it.
 */
export function filterSheet(choose: (show: Show) => void): FilterSheet {
  const group = filterGroup("show-sheet", choose, { autofocus: true });
  const badge = el(
    "span",
    { className: "cp-badge" },
    "1",
    el("span", { className: "cp-visually-hidden" }, " on"),
  );
  const toggle = el(
    "button",
    {
      className: "cp-button cp-rail-toggle",
      type: "button",
      commandfor: "messages-filters",
      command: "show-modal",
    },
    icon("funnel"),
    "Filters",
  );
  const clear = el(
    "button",
    { className: "cp-button cp-rail-toggle", type: "button" },
    "Clear filters",
  );
  clear.addEventListener("click", () => {
    choose("all");
  });
  const clearAll = el("button", { className: "cp-button", type: "button" }, "Clear all");
  clearAll.addEventListener("click", () => {
    choose("all");
  });
  const dialog = el(
    "dialog",
    {
      className: "cp-dialog",
      "data-variant": "sheet",
      id: "messages-filters",
      "aria-labelledby": "messages-filters-title",
    },
    el(
      "form",
      { method: "dialog" },
      el(
        "header",
        null,
        el("h2", { id: "messages-filters-title" }, "Filters"),
        el(
          "button",
          {
            className: "cp-button",
            "data-variant": "quiet",
            type: "button",
            commandfor: "messages-filters",
            command: "close",
            "aria-label": "Close",
            "data-cp-tip": "Close",
          },
          icon("x"),
        ),
      ),
      el("div", { className: "cp-dialog-body" }, group.element),
      el(
        "footer",
        null,
        clearAll,
        el("button", { className: "cp-button", "data-variant": "primary", type: "submit" }, "Done"),
      ),
    ),
  ) as HTMLDialogElement;
  return {
    toggle,
    dialog,
    set(show, counts) {
      group.set(show, counts);
      if (show === "all") {
        badge.remove();
        clear.remove();
        return;
      }
      // A re-seat blurs, and Clear filters may hold the focus.
      if (badge.parentNode !== toggle) {
        toggle.append(badge);
      }
      if (toggle.nextSibling !== clear) {
        toggle.after(clear);
      }
    },
  };
}
