import { el, KEY_ATTR, reconcile } from "@cplieger/reactive/dom";
import { enhanceFeed, type Feed } from "@cplieger/ui/feed";
import { renderMarkdown } from "@cplieger/ui/markdown";

import { dayStamp, dayWords } from "../format.js";
import { icon } from "../icons.js";
import type { Post } from "../wire/types.gen.js";
import { agentLink } from "./names.js";
import { relativeTime } from "./parts.js";
import { readSummary } from "./readers.js";

const skeletonEntries = 3;

/** A post the user sent from this page that the conversation does not show yet. */
export interface Pending {
  /** Unique within the conversation for the page's life. */
  readonly n: number;
  readonly subject: string;
  readonly body: string;
  readonly at: number;
  readonly state: "sending" | "failed" | "sent";
  /** The stored message, once sent: its entry replaces this one. */
  readonly message: number | null;
}

type Item =
  | { readonly kind: "day"; readonly at: number }
  | { readonly kind: "marker" }
  | { readonly kind: "post"; readonly post: Post }
  | { readonly kind: "pending"; readonly pending: Pending };

interface Draw {
  /** The post the "New since your last visit" marker goes before, or null. */
  readonly marker: number | null;
  /** The conversation's path, which an origin chip names a subpath under. */
  readonly path: string | null;
  /** The user's own posts on their way, after every post. */
  readonly pending?: readonly Pending[];
  /** Retry on a failed pending entry. */
  readonly retry?: (n: number) => void;
}

interface ConversationFeed {
  readonly list: HTMLOListElement;
  /** The .cp-feed-foot pinned under it, holding the New messages button. */
  readonly foot: HTMLElement;
  /** Wires the feed's scroll keeping, once list and foot are in the page. */
  start(): Feed;
  /** Loading entries, until draw. */
  skeleton(): void;
  /** Draws posts, oldest first, with day separators and the marker. */
  draw(posts: readonly Post[], how: Draw): void;
  /** The entry of post id, or null. */
  entry(id: number): HTMLElement | null;
  /** Runs fn each time the reader reaches the newest entry after arrivals. */
  onCaughtUp(fn: () => void): void;
}

function items(posts: readonly Post[], how: Draw): Item[] {
  const { marker } = how;
  const out: Item[] = [];
  let day: string | null = null;
  for (const post of posts) {
    const stamp = dayStamp(post.at_ms);
    if (stamp !== day) {
      out.push({ kind: "day", at: post.at_ms });
      day = stamp;
    }
    if (post.id === marker) {
      out.push({ kind: "marker" });
    }
    out.push({ kind: "post", post });
  }
  const shown = new Set(posts.map((p) => p.id));
  for (const pending of how.pending ?? []) {
    if (pending.message === null || !shown.has(pending.message)) {
      out.push({ kind: "pending", pending });
    }
  }
  return out;
}

function keyOf(item: Item): string {
  switch (item.kind) {
    case "day":
      return `day:${dayStamp(item.at)}`;
    case "marker":
      return "marker";
    case "post":
      return `post:${String(item.post.id)}`;
    case "pending":
      return `pending:${String(item.pending.n)}`;
  }
}

/** Where under the conversation's path a post came from, as its chip names it. */
function originWords(origin: string, path: string | null): string {
  return path !== null && origin.startsWith(`${path}/`) ? origin.slice(path.length + 1) : origin;
}

/** Where a history entry was posted: its conversation's title and address. */
interface Where {
  readonly words: string;
  readonly href: string;
  readonly mono: boolean;
}

interface EntryHow {
  /** The conversation's path, which an origin chip names a subpath under. */
  readonly path: string | null;
  readonly idPrefix: string;
  readonly signal: AbortSignal;
  /** The entry's conversation, as a link chip, for a feed across conversations. */
  readonly posted?: Where;
}

/** One stored post as a .cp-feed-entry, with its read summary when it reached anyone. */
export function postEntry(
  post: Post,
  how: EntryHow,
): { readonly li: HTMLElement; readonly summary: ReturnType<typeof readSummary> | null } {
  const { signal } = how;
  const time = relativeTime(post.at_ms, signal);
  const header = el("header", null, agentLink(post.from));
  if (how.posted !== undefined) {
    const words = el("a", { href: how.posted.href, dir: "auto" }, how.posted.words);
    words.classList.toggle("cp-mono", how.posted.mono);
    header.append(el("span", { className: "cp-chip" }, words));
  }
  if (post.origin !== undefined) {
    header.append(
      el(
        "span",
        { className: "cp-chip" },
        el("span", { className: "cp-mono", dir: "auto" }, originWords(post.origin, how.path)),
      ),
    );
  }
  header.append(time);
  const body = el("div", { className: "cp-prose", dir: "auto" });
  renderMarkdown(body, post.body, { headingLevel: 4, signal });
  const footer = el("footer");
  let summary: ReturnType<typeof readSummary> | null = null;
  if (post.read.total === 0) {
    footer.append(el("span", { className: "cp-meta" }, "Reached no one."));
  } else {
    summary = readSummary(post.id, post.read, how.idPrefix, signal);
    footer.append(...summary.nodes);
  }
  const li = el(
    "li",
    { className: "cp-feed-entry" },
    header,
    el("h3", { dir: "auto" }, post.subject),
    body,
    footer,
  );
  li.toggleAttribute("data-own", post.from_kind === "user");
  return { li, summary };
}

/** A day separator before the entries of at's local day. */
export function dayItem(at: number): HTMLElement {
  return el(
    "li",
    { className: "cp-feed-day" },
    el("time", { datetime: dayStamp(at) }, dayWords(at)),
  );
}

/** A loading entry. */
export function skeletonEntry(): HTMLElement {
  return el(
    "li",
    { className: "cp-feed-entry", "data-skeleton": "" },
    ...Array.from({ length: 4 }, () => el("span", { className: "cp-skeleton" })),
  );
}

/**
 * An open conversation's messages as @cplieger/ui/feed draws them, each
 * body rendered by @cplieger/ui/markdown.
 */
export function conversationFeed(idPrefix: string, signal: AbortSignal): ConversationFeed {
  const list = el("ol", { className: "cp-feed", "aria-label": "Messages" }) as HTMLOListElement;
  const jump = el(
    "button",
    { className: "cp-button cp-feed-jump", "data-size": "sm", type: "button", hidden: true },
    icon("arrow-down"),
    el("span", { className: "cp-roll" }),
  ) as HTMLButtonElement;
  const foot = el("div", { className: "cp-feed-foot" }, jump);
  const summaries = new Map<number, ReturnType<typeof readSummary>>();
  let path: string | null = null;
  let retryPending: ((n: number) => void) | undefined;
  const drawnState = new WeakMap<HTMLElement, Pending["state"]>();

  const entry = (post: Post): HTMLElement => {
    const drawn = postEntry(post, { path, idPrefix, signal });
    if (drawn.summary !== null) {
      summaries.set(post.id, drawn.summary);
    }
    return drawn.li;
  };

  const pendingFooter = (li: HTMLElement, p: Pending): void => {
    if (drawnState.get(li) === p.state) {
      return;
    }
    drawnState.set(li, p.state);
    const footer = li.querySelector(":scope > footer");
    if (p.state === "sent") {
      li.removeAttribute("data-state");
      li.removeAttribute("aria-busy");
      footer?.replaceChildren();
      return;
    }
    li.setAttribute("data-state", p.state);
    if (p.state === "sending") {
      li.setAttribute("aria-busy", "true");
      footer?.replaceChildren(
        el(
          "span",
          { className: "cp-inline-load" },
          el("span", { className: "cp-loader", "aria-hidden": "true" }, el("span")),
          "Sending…",
        ),
      );
      return;
    }
    li.removeAttribute("aria-busy");
    const retry = el(
      "button",
      { className: "cp-button", "data-size": "sm", type: "button" },
      "Retry",
    );
    retry.addEventListener("click", () => {
      retryPending?.(p.n);
    });
    footer?.replaceChildren(
      el("span", { className: "cp-state", "data-tone": "danger" }, icon("circle-x"), "Not sent."),
      retry,
    );
  };

  const pendingEntry = (p: Pending): HTMLElement => {
    const time = relativeTime(p.at, signal);
    const body = el("div", { className: "cp-prose", dir: "auto" });
    renderMarkdown(body, p.body, { headingLevel: 4, signal });
    const li = el(
      "li",
      { className: "cp-feed-entry", "data-own": "" },
      el("header", null, agentLink("user"), time),
      el("h3", { dir: "auto" }, p.subject),
      body,
      el("footer"),
    );
    pendingFooter(li, p);
    return li;
  };

  return {
    list,
    foot,
    start: () => enhanceFeed(list, { foot, jump, signal }),
    skeleton() {
      list.setAttribute("aria-busy", "true");
      list.replaceChildren(...Array.from({ length: skeletonEntries }, skeletonEntry));
    },
    draw(posts, how) {
      path = how.path;
      retryPending = how.retry;
      for (const child of [...list.children]) {
        if (!child.hasAttribute(KEY_ATTR)) {
          child.remove();
        }
      }
      const shown = new Set(posts.map((p) => p.id));
      for (const id of summaries.keys()) {
        if (!shown.has(id)) {
          summaries.delete(id);
        }
      }
      reconcile<Item>(list, items(posts, how), {
        key: keyOf,
        mount: (item) => {
          switch (item.kind) {
            case "day":
              return dayItem(item.at);
            case "marker":
              return el("li", { className: "cp-since" }, "New since your last visit");
            case "post":
              return entry(item.post);
            case "pending":
              return pendingEntry(item.pending);
          }
        },
        update: (node, item) => {
          if (item.kind === "post") {
            summaries.get(item.post.id)?.update(item.post.read);
          } else if (item.kind === "pending") {
            pendingFooter(node, item.pending);
          }
        },
      });
      list.removeAttribute("aria-busy");
    },
    entry(id) {
      return list.querySelector<HTMLElement>(`[${KEY_ATTR}="post:${String(id)}"]`);
    },
    onCaughtUp(fn) {
      let showing = !jump.hidden;
      const watch = new MutationObserver(() => {
        if (showing && jump.hidden) {
          fn();
        }
        showing = !jump.hidden;
      });
      watch.observe(jump, { attributes: true, attributeFilter: ["hidden"] });
      signal.addEventListener("abort", () => {
        watch.disconnect();
      });
    },
  };
}
