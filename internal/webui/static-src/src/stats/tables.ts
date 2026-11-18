import { el } from "@cplieger/reactive/dom";
import { enhanceRankedBars } from "@cplieger/ui/ranked-bars";

import { durationWords } from "../format.js";
import { agentLink, conversationTitle } from "../messages/names.js";
import { routeHref } from "../router.js";
import type { Stats } from "../wire/types.gen.js";

type Cell = readonly [content: Node | string, attrs?: Readonly<Record<string, string>>];

/** A count drawn with its ranked bar, compared against the column cells sharing bar. */
function barCell(n: number, bar: string, extra: Readonly<Record<string, string>> = {}): Cell {
  return [
    el("data", { value: String(n) }, new Intl.NumberFormat().format(n)),
    { "data-bar": bar, ...extra },
  ];
}

/**
 * A ranked .cp-table under the heading labelledBy names: built on the
 * first draw, its body redrawn only when its rows change, so a refresh
 * leaves a focused link alone.
 */
function rankedTable(opts: {
  readonly labelledBy: string;
  readonly head: readonly Cell[];
  readonly empty: HTMLElement;
  readonly signal: AbortSignal;
}): { readonly element: HTMLElement; draw(rows: readonly (readonly Cell[])[], key: string): void } {
  const body = el("tbody");
  const table = el(
    "table",
    { className: "cp-table", "aria-labelledby": opts.labelledBy },
    el(
      "thead",
      null,
      el(
        "tr",
        null,
        ...opts.head.map(([words, attrs]) => el("th", { scope: "col", ...attrs }, words)),
      ),
    ),
    body,
  ) as HTMLTableElement;
  const panel = el("div", { className: "cp-panel", "data-flush": "" }, table);
  const element = el("div", null, opts.empty, panel);
  enhanceRankedBars(table, { signal: opts.signal });
  let shown: string | null = null;
  return {
    element,
    draw(rows, key) {
      opts.empty.hidden = rows.length > 0;
      panel.hidden = rows.length === 0;
      if (key === shown) {
        return;
      }
      shown = key;
      body.replaceChildren(
        ...rows.map(([first, ...rest]) =>
          el(
            "tr",
            null,
            el("th", { scope: "row", ...first?.[1] }, first?.[0] ?? ""),
            ...rest.map(([content, attrs]) => el("td", { ...attrs }, content)),
          ),
        ),
      );
    },
  };
}

/** "Who posts and reads": each agent's posts, reads, longest wait and held turns, most posts first. */
export function agentsTable(
  signal: AbortSignal,
  empty: HTMLElement,
): {
  readonly element: HTMLElement;
  draw(s: Stats): void;
} {
  const table = rankedTable({
    labelledBy: "stats-agents",
    head: [
      ["Agent"],
      ["Sent", { "data-bar": "mail" }],
      ["Read", { "data-bar": "mail" }],
      ["Longest wait", { "data-priority": "mid" }],
      ["Held turns", { "data-priority": "low" }],
    ],
    empty,
    signal,
  });
  return {
    element: table.element,
    draw(s) {
      const held = new Map(s.held_turns.map((h) => [h.name, h.turns]));
      const rows = s.agents.map((a): readonly Cell[] => [
        [agentLink(a.name)],
        barCell(a.posts, "mail"),
        barCell(a.read, "mail", { "data-series": "2" }),
        [a.read === 0 ? "No reads" : durationWords(a.longest_wait_ms), { "data-priority": "mid" }],
        [new Intl.NumberFormat().format(held.get(a.name) ?? 0), { "data-priority": "low" }],
      ]);
      table.draw(
        rows,
        JSON.stringify(
          s.agents.map((a) => [a.name, a.posts, a.read, a.longest_wait_ms, held.get(a.name)]),
        ),
      );
    },
  };
}

/** "Threads": each conversation with a post in the range, most posts first. */
export function threadsTable(
  signal: AbortSignal,
  empty: HTMLElement,
): {
  readonly element: HTMLElement;
  draw(s: Stats): void;
} {
  const table = rankedTable({
    labelledBy: "stats-threads",
    head: [
      ["Thread"],
      ["Posts", { "data-bar": "posts" }],
      ["Participants", { "data-bar": "participants", "data-priority": "mid" }],
      ["Deliveries", { "data-bar": "deliveries", "data-priority": "low" }],
    ],
    empty,
    signal,
  });
  return {
    element: table.element,
    draw(s) {
      const rows = s.threads.map((t): readonly Cell[] => [
        [
          el(
            "a",
            {
              href: routeHref({ screen: "messages", show: "all", conversation: t.thread.id }),
              dir: "auto",
              ...(t.thread.kind === "path" ? { className: "cp-mono" } : {}),
            },
            conversationTitle(t.thread),
          ),
        ],
        barCell(t.posts, "posts"),
        barCell(t.participants, "participants", { "data-priority": "mid" }),
        barCell(t.deliveries, "deliveries", { "data-priority": "low" }),
      ]);
      table.draw(rows, JSON.stringify(s.threads));
    },
  };
}
