import { el, KEY_ATTR, reconcile } from "@cplieger/reactive/dom";
import { announce } from "@cplieger/ui/announce";
import { enhanceLoadMore } from "@cplieger/ui/load-more";

import { dayStamp, retentionWords } from "../format.js";
import { icon } from "../icons.js";
import { routeHref } from "../router.js";
import { failureWords, type ApiResult } from "../transport.js";
import { getAgentHistory } from "../wire/client.gen.js";
import type { HistoryPost } from "../wire/types.gen.js";
import { dayItem, postEntry, skeletonEntry } from "./feed.js";
import { conversationTitle } from "./names.js";
import { busyButton, retryButton } from "./parts.js";
import { reader, type Answer } from "./reader.js";
import { rewalk } from "./window.js";
import type { readSummary } from "./readers.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

interface HistoryOptions {
  readonly name: string;
  readonly signal: AbortSignal;
  /** How long history is kept, for the empty history's words; null when unknown. */
  readonly retention: Promise<number | null>;
}

interface AgentHistory {
  /** The "History" section: its heading, then the feed. */
  readonly section: HTMLElement;
  /** Loads the newest page; until then the section holds its heading alone. */
  load(): void;
  /** A conversation changed: reads every page shown again. */
  changed(): Promise<void>;
  /** Reads again under outer, a resync's signal. */
  resync(outer: AbortSignal): Promise<void>;
}

/** What the history holds: every page it loaded, newest first. */
interface Held {
  readonly posts: readonly HistoryPost[];
  /** The before of the next older page, or undefined at the oldest. */
  readonly next: number | undefined;
}

type Item =
  | { readonly kind: "day"; readonly at: number }
  | { readonly kind: "post"; readonly post: HistoryPost };

/** Newest first, a day separator before each local day's newest post. */
function items(posts: readonly HistoryPost[]): Item[] {
  const out: Item[] = [];
  let day: string | null = null;
  for (const post of posts) {
    const stamp = dayStamp(post.at_ms);
    if (stamp !== day) {
      out.push({ kind: "day", at: post.at_ms });
      day = stamp;
    }
    out.push({ kind: "post", post });
  }
  return out;
}

const keyOf = (item: Item): string =>
  item.kind === "day" ? `day:${dayStamp(item.at)}` : `post:${String(item.post.id)}`;

/**
 * An agent's history (shape 2): every post it sent or received, newest
 * first, each naming its conversation as a link, with Show older at the end.
 */
export function agentHistory(opts: HistoryOptions): AgentHistory {
  const { name, signal } = opts;
  const list = el("ol", { className: "cp-feed", "aria-label": `Messages of @${name}` });
  const older = busyButton("Show older");
  const foot = el("div", { className: "cp-list-foot" }, older);
  const body = el("div", { className: "cp-stack" });
  const section = el("section", null, el("h2", null, "History"), body);
  const summaries = new Map<number, ReturnType<typeof readSummary>>();

  let held: Held = { posts: [], next: undefined };
  /** held is on screen, so a read refreshes it rather than loading. */
  let loaded = false;
  let staleNotice: HTMLElement | null = null;

  const entry = (post: HistoryPost): HTMLElement => {
    const t = post.conversation;
    const drawn = postEntry(post, {
      path: t.kind === "path" ? (t.path ?? null) : null,
      idPrefix: "agent",
      signal,
      posted: {
        words: conversationTitle(t),
        href: routeHref({ screen: "messages", show: "all", conversation: t.id }),
        mono: t.kind === "path",
      },
    });
    if (drawn.summary !== null) {
      summaries.set(post.id, drawn.summary);
    }
    return drawn.li;
  };

  const empty = async (): Promise<void> => {
    const retention = await opts.retention;
    if (signal.aborted || held.posts.length > 0) {
      return;
    }
    body.replaceChildren(
      el(
        "div",
        { className: "cp-empty" },
        el(
          "strong",
          null,
          retention === null
            ? "No messages."
            : `No messages in the last ${retentionWords(retention)}.`,
        ),
      ),
    );
  };

  const draw = (): void => {
    loaded = true;
    if (held.posts.length === 0) {
      void empty();
      return;
    }
    const shown = new Set(held.posts.map((p) => p.id));
    for (const id of summaries.keys()) {
      if (!shown.has(id)) {
        summaries.delete(id);
      }
    }
    reconcile<Item>(list, items(held.posts), {
      key: keyOf,
      mount: (item) => (item.kind === "day" ? dayItem(item.at) : entry(item.post)),
      update: (_node, item) => {
        if (item.kind === "post") {
          summaries.get(item.post.id)?.update(item.post.read);
        }
      },
    });
    list.removeAttribute("aria-busy");
    const parts: HTMLElement[] = [
      ...(staleNotice === null ? [] : [staleNotice]),
      list,
      ...(held.next === undefined ? [] : [foot]),
    ];
    if (parts.some((p, i) => body.children[i] !== p) || body.children.length !== parts.length) {
      const focused = document.activeElement;
      body.replaceChildren(...parts);
      if (focused instanceof HTMLElement && focused !== document.body && body.contains(focused)) {
        focused.focus({ preventScroll: true });
      }
    }
  };

  const failed = (failure: Failure): void => {
    if (loaded) {
      setStale(failure);
      return;
    }
    body.replaceChildren(
      el(
        "div",
        { className: "cp-notice", "data-tone": "danger" },
        icon("circle-x"),
        el("p", null, `History could not be loaded. ${failureWords(failure)}`),
        retryButton(() => {
          void reads.retry();
        }),
      ),
    );
  };

  const setStale = (failure: Failure | null): void => {
    if (failure === null) {
      staleNotice?.remove();
      staleNotice = null;
      return;
    }
    staleNotice ??= el("div", { className: "cp-notice", "data-tone": "danger" });
    staleNotice.replaceChildren(
      icon("circle-x"),
      el("p", null, `History could not be refreshed. ${failureWords(failure)}`),
      retryButton(() => {
        void reads.retry();
      }),
    );
    if (!staleNotice.isConnected) {
      body.prepend(staleNotice);
      announce("History could not be refreshed.");
    }
  };

  const loadFirst = async (asking: AbortSignal): Promise<Answer> => {
    loaded = false;
    const result = await getAgentHistory({ name }, { signal: asking });
    if (!result.ok) {
      return result.code === "aborted" ? null : { failure: result };
    }
    return {
      draw: () => {
        for (const child of [...list.children]) {
          if (!child.hasAttribute(KEY_ATTR)) {
            child.remove();
          }
        }
        setStale(null);
        held = { posts: result.data.posts, next: result.data.next };
        draw();
        return false;
      },
    };
  };

  /** Reads every page held again. */
  const walk = async (asking: AbortSignal): Promise<Answer> => {
    const before = held;
    const result = await getAgentHistory({ name }, { signal: asking });
    if (!result.ok) {
      return result.code === "aborted" ? null : { failure: result };
    }
    const walked = await rewalk<HistoryPost, number>({
      held: { items: before.posts, next: before.next },
      fresh: { items: result.data.posts, next: result.data.next },
      more: async (cursor) => {
        const page = await getAgentHistory({ name, before: cursor }, { signal: asking });
        return page.ok
          ? { ok: true, data: { items: page.data.posts, next: page.data.next } }
          : page;
      },
      order: (a, b) => b.id - a.id,
      id: (p) => p.id,
      cursorOf: (p) => p.id,
    });
    if (walked === null) {
      return null;
    }
    if (walked.failure !== null) {
      return { failure: walked.failure };
    }
    const { window } = walked;
    return {
      draw: () => {
        if (held !== before) {
          return true;
        }
        setStale(null);
        held = { posts: window.items, next: window.next };
        draw();
        return false;
      },
    };
  };

  const reads = reader(signal, {
    holds: () => loaded,
    load: loadFirst,
    refresh: walk,
    skeleton: () => {
      list.setAttribute("aria-busy", "true");
      list.replaceChildren(...Array.from({ length: 3 }, skeletonEntry));
      body.replaceChildren(list);
    },
    failed,
  });

  enhanceLoadMore(older, {
    list,
    words: {
      count: (n) => (n === 1 ? "1 older message" : `${String(n)} older messages`),
      failed: "Older messages could not be loaded.",
    },
    signal,
    load: async (asking) => {
      const before = held.next;
      if (before === undefined) {
        return { items: [], more: false };
      }
      const result = await getAgentHistory({ name, before }, { signal: asking });
      if (!result.ok) {
        throw new Error(failureWords(result));
      }
      if (held.next !== before) {
        return { items: [], more: held.next !== undefined };
      }
      const ids = new Set(held.posts.map((p) => p.id));
      const added = result.data.posts.filter((p) => !ids.has(p.id));
      held = { posts: [...held.posts, ...added], next: result.data.next };
      draw();
      const items = added.flatMap(
        (p) => list.querySelector<HTMLElement>(`[${KEY_ATTR}="post:${String(p.id)}"]`) ?? [],
      );
      return { items, more: held.next !== undefined };
    },
  });

  return {
    section,
    load() {
      void reads.load();
    },
    changed: () => reads.changed(),
    resync: (outer) => reads.resync(outer),
  };
}
