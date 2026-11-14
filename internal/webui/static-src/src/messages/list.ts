import { el } from "@cplieger/reactive/dom";
import { announce } from "@cplieger/ui/announce";
import { enhanceHeldList } from "@cplieger/ui/held-list";
import { enhanceKeyedList } from "@cplieger/ui/keyed-list";
import { enhanceLoadMore } from "@cplieger/ui/load-more";
import { enhanceRelativeTime } from "@cplieger/ui/relative-time";

import { retentionWords } from "../format.js";
import { icon } from "../icons.js";
import type { Live } from "../live.js";
import { failureWords, type ApiResult } from "../transport.js";
import { listAgents, listConversations } from "../wire/client.gen.js";
import type {
  ConversationCounts,
  ConversationRow,
  ConversationsPage,
  Show,
} from "../wire/types.gen.js";
import { countOf } from "./filters.js";
import { conversationTitle, senderWords } from "./names.js";
import { busyButton, retryButton } from "./parts.js";
import { reader, type Answer } from "./reader.js";
import { rewalk } from "./window.js";

/** Where docs/client.md tells an operator how to set up a machine. */
const setupGuide = "https://github.com/cplieger/grelot/blob/main/docs/client.md";

const skeletonRows = 6;

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

interface Cursor {
  readonly before: number;
  readonly before_thread: number;
}

/** What the list holds: the rows of every page it loaded, and the next page's cursor. */
interface Rows {
  readonly rows: readonly ConversationRow[];
  readonly next: Cursor | null;
}

type FirstPage =
  | { readonly kind: "page"; readonly page: ConversationsPage; readonly firstRun: boolean }
  | { readonly kind: "failed"; readonly failure: Failure };

interface ListOptions {
  readonly signal: AbortSignal;
  readonly live: Live;
  /** How long history is kept, for the empty list's words; null when unknown. */
  readonly retention: Promise<number | null>;
  /** The address that opens conversation id under the list's filter. */
  readonly href: (id: number) => string;
  /** Every answer's counts, for the filters. */
  readonly onCounts: (counts: ConversationCounts) => void;
  /** A load settled, so whether the list names the open conversation is known. */
  readonly onSettled: () => void;
  readonly clearFilters: () => void;
  /** Opens the New message dialog. */
  readonly newMessage: () => void;
}

interface ConversationList {
  /** The list column of the list-detail. */
  readonly element: HTMLElement;
  /** Loads show's newest page in place of what the list holds; announce says how many. */
  load(show: Show, opts: { readonly announce: boolean }): void;
  /** Marks conversation id's row as open, or none. */
  select(id: number | null): void;
  row(id: number): ConversationRow | undefined;
}

/** The server's order: newest last post first, then the newer thread. */
function newerFirst(a: ConversationRow, b: ConversationRow): number {
  return b.last.id - a.last.id || b.id - a.id;
}

function cursorOf(page: ConversationsPage): Cursor | null {
  return page.next === undefined || page.next_thread === undefined
    ? null
    : { before: page.next, before_thread: page.next_thread };
}

/** The rows of next whose last post the reader has not been shown. */
function arrivals(
  shown: readonly ConversationRow[],
  next: readonly ConversationRow[],
): ConversationRow[] {
  const lastOf = new Map(shown.map((r) => [r.id, r.last.id]));
  return next.filter((r) => lastOf.get(r.id) !== r.last.id);
}

function heldWords(n: number): string {
  return n === 1
    ? "New messages in 1 conversation."
    : `New messages in ${String(n)} conversations.`;
}

/** A drawn row's parts that change with its conversation. */
interface RowParts {
  readonly link: HTMLElement;
  readonly title: HTMLElement;
  readonly preview: HTMLElement;
  readonly time: HTMLTimeElement;
  readonly participants: Text;
  readonly participantsWord: HTMLElement;
  readonly unread: Text;
  readonly badge: HTMLElement;
  readonly counts: HTMLElement;
  /** The last post's time the clock was started for. */
  at: number | null;
  /** Stops the relative time's updates for the current at. */
  clock: AbortController | null;
}

function setText(node: Node, words: string): void {
  if (node.textContent !== words) {
    node.textContent = words;
  }
}

function setAttr(element: Element, name: string, value: string): void {
  if (element.getAttribute(name) !== value) {
    element.setAttribute(name, value);
  }
}

function sameOrder(a: readonly ConversationRow[], b: readonly ConversationRow[]): boolean {
  return a.length === b.length && a.every((r, i) => r.id === b[i]?.id);
}

function countWords(n: number): string {
  if (n === 0) {
    return "No conversations";
  }
  return n === 1 ? "1 conversation" : `${String(n)} conversations`;
}

/** The empty list's sentence, naming the filter and how long history is kept. */
function emptyWords(show: Show, retention: number | null): string {
  const what = {
    all: "No conversations",
    threads: "No threads",
    direct: "No direct conversations",
  }[show];
  return retention === null ? `${what}.` : `${what} in the last ${retentionWords(retention)}.`;
}

function skeleton(): HTMLElement {
  const rows = Array.from({ length: skeletonRows }, () =>
    el(
      "li",
      null,
      el(
        "div",
        { className: "cp-list-row", "data-skeleton": "" },
        el(
          "span",
          { className: "cp-cell-stack" },
          el("span", { className: "cp-skeleton" }),
          el("span", { className: "cp-skeleton" }),
        ),
      ),
    ),
  );
  return el(
    "ul",
    { className: "cp-list", "aria-label": "Conversations", "aria-busy": "true" },
    ...rows,
  );
}

/**
 * The Messages list (shape 1): one row per conversation, newest activity
 * first, 50 a page with Show older, its loading, empty, error and first-run
 * states, and the live stream's refreshes, held back while the reader is in
 * the list (design-patterns.md "Lists").
 */
export function conversationList(opts: ListOptions): ConversationList {
  const { signal, live } = opts;
  const element = el("div", { className: "cp-stack" });
  const list = el("ul", {
    className: "cp-list",
    "aria-label": "Conversations",
  }) as HTMLUListElement;
  let body: HTMLElement = el("div");
  element.append(body);

  let show: Show = "all";
  let held: Rows = { rows: [], next: null };
  let selected: number | null = null;
  let retention: number | null = null;
  /** The newest refresh held back while the reader is in the list; the offer's first change shows it. */
  let waiting: { readonly rows: Rows; readonly counts: ConversationCounts } | null = null;
  /** Bumped by each load, so a Show older answer for an earlier load is dropped. */
  let generation = 0;
  /** The next load's drawn page says how many conversations it holds. */
  let announcing = false;
  let refreshNotice: HTMLElement | null = null;
  let state: "loading" | "rows" | "empty" | "failed" = "loading";
  let firstRun = false;

  const settings = opts.retention.then((ms) => {
    retention = ms;
  });

  const setBody = (next: HTMLElement): void => {
    if (body !== next) {
      body.replaceWith(next);
      body = next;
    }
  };

  /** Each drawn row's parts, by conversation id, which a refresh rewrites in place. */
  const drawn = new Map<number, RowParts>();

  const fill = (p: RowParts, row: ConversationRow): void => {
    setAttr(p.link, "href", opts.href(row.id));
    if (row.id === selected) {
      setAttr(p.link, "aria-current", "true");
    } else {
      p.link.removeAttribute("aria-current");
    }
    p.link.toggleAttribute("data-unread", row.unread > 0);
    const words = conversationTitle(row);
    if (row.kind === "path") {
      setAttr(p.title, "data-cp-tip", words);
    }
    setText(p.title, words);
    const text = row.last.preview === "" ? row.last.subject : row.last.preview;
    setText(p.preview, `${senderWords(row.last.from)}: ${text}`);
    // Each enhanceRelativeTime call holds a listener on its signal until it aborts.
    if (p.at !== row.last.at_ms) {
      p.clock?.abort();
      p.clock = new AbortController();
      p.at = row.last.at_ms;
      p.time.dateTime = new Date(row.last.at_ms).toISOString();
      enhanceRelativeTime(p.time, {
        style: "narrow",
        signal: AbortSignal.any([signal, p.clock.signal]),
      });
    }
    setText(p.participants, String(row.participants));
    setText(p.participantsWord, row.participants === 1 ? " participant" : " participants");
    if (row.unread > 0) {
      setText(p.unread, String(row.unread));
      if (p.badge.parentNode !== p.counts) {
        p.counts.append(p.badge);
      }
    } else {
      p.badge.remove();
    }
  };

  const mountRow = (row: ConversationRow): HTMLElement => {
    const participants = document.createTextNode("");
    const unread = document.createTextNode("");
    const p: RowParts = {
      link: el("a", { className: "cp-list-row" }),
      // A conversation keeps its kind, so its title's form is set once.
      title:
        row.kind === "path"
          ? el("span", {
              className: "cp-truncate cp-mono",
              "data-cut": "start",
              dir: "ltr",
            })
          : el("span", { className: "cp-truncate", dir: "auto" }),
      preview: el("small", { className: "cp-truncate", dir: "auto" }),
      time: el("time") as HTMLTimeElement,
      participants,
      participantsWord: el("span", { className: "cp-visually-hidden" }),
      unread,
      badge: el(
        "span",
        { className: "cp-badge" },
        unread,
        el("span", { className: "cp-visually-hidden" }, " unread"),
      ),
      counts: el("span", { className: "cp-cluster" }),
      at: null,
      clock: null,
    };
    p.counts.append(
      el("span", { className: "cp-meta" }, icon("users"), participants, p.participantsWord),
    );
    p.link.append(
      el("span", { className: "cp-cell-stack" }, p.title, p.preview),
      el("span", { "data-part": "end" }, p.time, p.counts),
    );
    drawn.set(row.id, p);
    fill(p, row);
    return el("li", null, p.link);
  };

  const older = busyButton("Show older");
  const foot = el("li", { className: "cp-list-foot" }, older);
  /** The rows the keyed list draws, by key. */
  let byKey = new Map<string, ConversationRow>();
  const keyed = enhanceKeyedList(list, {
    keys: [],
    window: false,
    signal,
    row: (key) => {
      const row = byKey.get(key);
      if (row === undefined) {
        throw new TypeError(`no conversation row for ${key}`);
      }
      return mountRow(row);
    },
  });
  const offer = enhanceHeldList(list, { words: { count: heldWords }, signal });

  const drawRows = (): void => {
    byKey = new Map(held.rows.map((r) => [String(r.id), r]));
    keyed.update([...byKey.keys()]);
    for (const row of held.rows) {
      const parts = drawn.get(row.id);
      if (parts !== undefined) {
        fill(parts, row);
      }
    }
    for (const [id, parts] of drawn) {
      if (!byKey.has(String(id))) {
        parts.clock?.abort();
        drawn.delete(id);
      }
    }
    if (held.next === null) {
      foot.remove();
    } else if (!foot.isConnected) {
      list.append(foot);
    }
  };

  const emptyState = (): HTMLElement => {
    if (firstRun) {
      return el(
        "div",
        { className: "cp-empty" },
        icon("inbox"),
        el("h2", null, "No agent has registered yet."),
        el("p", null, "Conversations show here once a machine's agents start posting."),
        el("a", { className: "cp-button", href: setupGuide }, "Set up a machine"),
      );
    }
    const empty = el(
      "div",
      { className: "cp-empty" },
      icon("inbox"),
      el("h2", null, emptyWords(show, retention)),
    );
    if (show !== "all") {
      const clear = el("button", { className: "cp-button", type: "button" }, "Clear filters");
      clear.addEventListener("click", opts.clearFilters);
      empty.append(clear);
    } else {
      const write = el(
        "button",
        { className: "cp-button", "data-variant": "primary", type: "button" },
        icon("pen-line"),
        "New message",
      );
      write.addEventListener("click", opts.newMessage);
      empty.append(write);
    }
    return empty;
  };

  const failedState = (failure: Failure): HTMLElement => {
    const retry = retryButton(() => {
      announcing = true;
      void reads.retry();
    });
    return el(
      "div",
      { className: "cp-notice", "data-tone": "danger" },
      icon("circle-x"),
      el(
        "div",
        null,
        el("p", null, el("strong", null, "Conversations could not be loaded.")),
        el("p", null, failureWords(failure)),
      ),
      retry,
    );
  };

  const dropPending = (): void => {
    offer.discard();
    waiting = null;
  };

  const setRefreshNotice = (failure: Failure | null): void => {
    refreshNotice?.remove();
    refreshNotice = null;
    if (failure === null) {
      return;
    }
    const retry = retryButton(() => {
      void reads.retry();
    });
    refreshNotice = el(
      "div",
      { className: "cp-notice", "data-tone": "danger" },
      icon("circle-x"),
      el(
        "p",
        null,
        `The list could not be refreshed. ${failure.message ?? "grelot did not answer."}`,
      ),
      retry,
    );
    element.prepend(refreshNotice);
    announce("The list could not be refreshed.");
  };

  const holds = (): boolean => state === "rows" || state === "empty";

  /** A failed read keeps what holds() shows under its notice; a failed load shows the failed state. */
  const failed = (failure: Failure): void => {
    if (holds()) {
      setRefreshNotice(failure);
      return;
    }
    state = "failed";
    setBody(failedState(failure));
    announce("Conversations could not be loaded.");
    opts.onSettled();
  };

  /** Shows rows, or the empty state when there are none. */
  const present = (rows: Rows, counts: ConversationCounts): void => {
    held = rows;
    opts.onCounts(counts);
    list.removeAttribute("aria-busy");
    if (rows.rows.length === 0) {
      state = "empty";
      setBody(emptyState());
    } else {
      state = "rows";
      drawRows();
      setBody(list);
    }
    opts.onSettled();
  };

  const firstPage = async (kind: Show, stop: AbortSignal): Promise<FirstPage | null> => {
    const result = await listConversations({ kind }, { signal: stop });
    if (!result.ok) {
      return result.code === "aborted" ? null : { kind: "failed", failure: result };
    }
    let none = false;
    // First run is a server no agent has reached and nobody has posted to.
    if (countOf("all", result.data.counts) === 0) {
      const agents = await listAgents({ signal: stop });
      none = agents.ok && agents.data.agents.length === 0;
    }
    await settings;
    return stop.aborted ? null : { kind: "page", page: result.data, firstRun: none };
  };

  /** Reads show's newest page in place of what the list holds. */
  const loadFirst = async (asking: AbortSignal): Promise<Answer> => {
    dropPending();
    setRefreshNotice(null);
    state = "loading";
    list.setAttribute("aria-busy", "true");
    const result = await firstPage(show, asking);
    if (result === null) {
      return null;
    }
    if (result.kind === "failed") {
      return { failure: result.failure };
    }
    return {
      draw: () => {
        firstRun = result.firstRun;
        present({ rows: result.page.rows, next: cursorOf(result.page) }, result.page.counts);
        if (announcing) {
          announcing = false;
          announce(`${countWords(countOf(show, result.page.counts))}.`);
        }
        return false;
      },
    };
  };

  /**
   * Shows rows through the offer, which holds them while the reader is in the
   * list; later refreshes replace what waits, so Show draws the newest.
   */
  const offerRows = (rows: Rows, counts: ConversationCounts, n: number): void => {
    waiting = { rows, counts };
    opts.onCounts(counts);
    offer.change(
      () => {
        const w = waiting;
        if (w === null) {
          return [];
        }
        waiting = null;
        const fresh = arrivals(held.rows, w.rows.rows);
        present(w.rows, w.counts);
        return fresh.flatMap((r) => keyed.rowOf(String(r.id)) ?? []);
      },
      Math.max(0, n - offer.held),
    );
  };

  /** Reads every page the list holds again, so each row shown is the server's current one. */
  const walk = async (asking: AbortSignal): Promise<Answer> => {
    const before = held;
    const result = await firstPage(show, asking);
    if (result === null) {
      return null;
    }
    if (result.kind === "failed") {
      return { failure: result.failure };
    }
    const kind = show;
    const walked = await rewalk<ConversationRow, Cursor>({
      held: { items: before.rows, next: before.next ?? undefined },
      fresh: { items: result.page.rows, next: cursorOf(result.page) ?? undefined },
      more: async (cursor) => {
        const page = await listConversations({ kind, ...cursor }, { signal: asking });
        return page.ok
          ? { ok: true, data: { items: page.data.rows, next: cursorOf(page.data) ?? undefined } }
          : page;
      },
      order: newerFirst,
      id: (r) => r.id,
      cursorOf: (r) => ({ before: r.last.id, before_thread: r.id }),
    });
    if (walked === null) {
      return null;
    }
    if (walked.failure !== null) {
      return { failure: walked.failure };
    }
    const next: Rows = { rows: walked.window.items, next: walked.window.next ?? null };
    return {
      draw: () => {
        if (held !== before) {
          return true;
        }
        setRefreshNotice(null);
        firstRun = result.firstRun;
        const n = arrivals(held.rows, next.rows).length;
        const moves = n > 0 && !sameOrder(held.rows, next.rows);
        if (state === "rows" && (offer.held > 0 || moves)) {
          offerRows(next, result.page.counts, n);
          return false;
        }
        dropPending();
        present(next, result.page.counts);
        return false;
      },
    };
  };

  const reads = reader(signal, {
    holds,
    load: loadFirst,
    refresh: walk,
    skeleton: () => {
      setBody(skeleton());
    },
    failed,
  });

  enhanceLoadMore(older, {
    list,
    words: {
      count: (n) => (n === 1 ? "1 older conversation" : `${String(n)} older conversations`),
      failed: "Older conversations could not be loaded.",
    },
    signal,
    load: async (asking) => {
      const cursor = held.next;
      if (cursor === null) {
        return { items: [], more: false };
      }
      const gen = generation;
      const result = await listConversations({ kind: show, ...cursor }, { signal: asking });
      if (gen !== generation || held.next !== cursor) {
        return { items: [], more: held.next !== null };
      }
      if (!result.ok) {
        throw new Error(failureWords(result));
      }
      const ids = new Set(held.rows.map((r) => r.id));
      const added = result.data.rows.filter((r) => !ids.has(r.id));
      const next = cursorOf(result.data);
      present({ rows: [...held.rows, ...added], next }, result.data.counts);
      // What waits was read before this page, so the page joins it now for a
      // Show pressed before the read below lands, which then replaces it.
      if (waiting !== null) {
        const have = new Set(waiting.rows.rows.map((r) => r.id));
        waiting = {
          ...waiting,
          rows: { rows: [...waiting.rows.rows, ...added.filter((r) => !have.has(r.id))], next },
        };
        void reads.changed();
      }
      return { items: added.flatMap((r) => keyed.rowOf(String(r.id)) ?? []), more: next !== null };
    },
  });

  live.watch(
    {
      changed: (change) => {
        if (change.conversations.length > 0 || (change.agents && firstRun)) {
          void reads.changed();
        }
      },
      resync: (outer) => reads.resync(outer),
    },
    signal,
  );

  return {
    element,
    load(next, how) {
      show = next;
      announcing = how.announce;
      generation++;
      void reads.load();
    },
    select(id) {
      selected = id;
      for (const [rowId, parts] of drawn) {
        if (rowId === id) {
          parts.link.setAttribute("aria-current", "true");
        } else {
          parts.link.removeAttribute("aria-current");
        }
      }
    },
    row(id) {
      return held.rows.find((r) => r.id === id);
    },
  };
}
