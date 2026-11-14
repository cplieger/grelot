import { el } from "@cplieger/reactive/dom";
import { announce } from "@cplieger/ui/announce";
import type { Feed } from "@cplieger/ui/feed";
import { enhanceLoadMore } from "@cplieger/ui/load-more";

import { retentionWords } from "../format.js";
import { icon } from "../icons.js";
import type { Live } from "../live.js";
import { failureWords, type ApiResult } from "../transport.js";
import { getConversation, markConversationRead } from "../wire/client.gen.js";
import type { Conversation, Post, PostTarget, Thread } from "../wire/types.gen.js";
import { conversationComposer, storedCount, type SendStore } from "./composer.js";
import { conversationContext } from "./context.js";
import { conversationFeed } from "./feed.js";
import { conversationTitle, isUser, kindWords, senderWords } from "./names.js";
import { busyButton, retryButton } from "./parts.js";
import { reader, type Answer } from "./reader.js";
import { rewalk } from "./window.js";

type Named = Pick<Thread, "kind" | "title" | "path" | "agents">;

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

interface DetailOptions {
  readonly signal: AbortSignal;
  /** The page's life, which a send runs on. */
  readonly page: AbortSignal;
  /** The page's sends, which outlive this view. */
  readonly sends: SendStore;
  readonly live: Live;
  /** How long history is kept, for the empty conversation's words; null when unknown. */
  readonly retention: Promise<number | null>;
  /** Opens the New message dialog to agent name. */
  readonly compose: (name: string) => void;
}

interface ConversationDetail {
  /** The list-detail's .cp-detail, hidden until the route opens one. */
  readonly section: HTMLElement;
  /** Opens conversation id, titled from known until the server answers. */
  show(id: number, known?: Named): void;
  /** The route closed the conversation: its live updates stop. */
  clear(): void;
  /** Moves focus to the open conversation's heading. */
  focus(): void;
}

/** What the open conversation holds: every page it loaded, oldest first. */
interface Held {
  readonly posts: readonly Post[];
  /** The before of the next older page, or undefined at the oldest. */
  readonly next: number | undefined;
}

/**
 * Where the user's reply goes: the thread itself, or for a direct
 * conversation the other side; null for two agents' conversation, which
 * the user cannot post in.
 */
function replyTarget(t: Thread): PostTarget | null {
  switch (t.kind) {
    case "announce":
      return { announce: true };
    case "path":
      return t.path === undefined ? null : { path: t.path };
    case "direct": {
      const sides = t.agents ?? [];
      const [other, ...more] = sides.filter((a) => !isUser(a));
      return other !== undefined && more.length === 0 ? { agent: other } : null;
    }
  }
}

function arrivalWords(posts: readonly Post[]): string {
  const names = [...new Set(posts.map((p) => senderWords(p.from)))];
  const who = new Intl.ListFormat("en", { type: "conjunction" }).format(names);
  return posts.length === 1 ? `New message from ${who}` : `New messages from ${who}`;
}

/**
 * The open conversation (shape 1 item 3): its heading, its context, its
 * messages oldest first with Show older, kept current by the live stream
 * and marked read once shown.
 */
export function conversationDetail(opts: DetailOptions): ConversationDetail {
  const heading = el("h2", { id: "conversation-title", tabIndex: -1, dir: "auto" });
  const chip = el("span", { className: "cp-chip" });
  const header = el(
    "header",
    null,
    heading,
    el(
      "button",
      {
        className: "cp-button",
        "data-variant": "quiet",
        type: "button",
        "data-detail-close": "",
        "aria-label": "Close",
        "data-cp-tip": "Close",
      },
      icon("x"),
    ),
  );
  const body = el("div", { className: "cp-detail-body" });
  const section = el(
    "section",
    { className: "cp-detail", hidden: true, "aria-labelledby": "conversation-title" },
    header,
    body,
  );

  let shown: number | null = null;
  /** The heading names shown's conversation, from the list or the server. */
  let titled = false;
  let view: AbortController | null = null;

  const placeholder = (): void => {
    heading.classList.remove("cp-mono");
    chip.remove();
    heading.replaceChildren(
      el("span", { className: "cp-skeleton", "data-variant": "heading" }),
      el("span", { className: "cp-visually-hidden" }, "Loading conversation…"),
    );
  };

  const title = (t: Named): void => {
    titled = true;
    heading.classList.toggle("cp-mono", t.kind === "path");
    const words = conversationTitle(t);
    if (heading.textContent !== words) {
      heading.textContent = words;
    }
    const kind = kindWords(t.kind);
    if (kind === null) {
      chip.remove();
    } else {
      chip.textContent = kind;
      if (heading.nextSibling !== chip) {
        heading.after(chip);
      }
    }
  };

  const gone = (): void => {
    titled = true;
    heading.classList.remove("cp-mono");
    chip.remove();
    heading.textContent = "This conversation is gone.";
    body.replaceChildren(el("p", null, "Its messages expired, or the address is wrong."));
  };

  const run = (id: number, signal: AbortSignal): void => {
    const feed = conversationFeed("conversation", signal);
    let held: Held = { posts: [], next: undefined };
    let marker: number | null = null;
    let path: string | null = null;
    /** A change to the agents moves a path thread's or a direct thread's context. */
    let hasContext = false;
    let frame: {
      readonly update: (c: Conversation) => void;
      readonly main: HTMLElement;
      readonly scroll: Feed;
    } | null = null;
    let refreshNotice: HTMLElement | null = null;
    let empty: HTMLElement | null = null;
    /** The conversation is gone, which no later read changes. */
    let ended = false;
    let reading = false;
    /** The newest post the reader saw while a mark-read was in flight, or 0. */
    let again = 0;
    /** The newest post a mark-read has covered. */
    let markedUpTo = 0;
    let composer: ReturnType<typeof conversationComposer> | null = null;
    /** storedCount when the read held now began. */
    let readFrom = 0;
    let replying = false;

    const older = busyButton("Show older");
    const head = el("div", { className: "cp-feed-head" }, older);

    /**
     * Marks read every post up to upTo, by default the newest held, which a
     * caller passes only while the reader is at the newest; markedUpTo moves
     * once the server stamped them.
     */
    const markRead = (upTo = held.posts.at(-1)?.id): void => {
      if (upTo === undefined) {
        return;
      }
      if (reading) {
        again = Math.max(again, upTo);
        return;
      }
      reading = true;
      again = 0;
      void markConversationRead({ id, body: { up_to: upTo } }, { signal }).then((result) => {
        reading = false;
        if (result.ok) {
          markedUpTo = Math.max(markedUpTo, upTo);
        }
        // The bound seen in flight, not the newest held now: a later post may sit unseen.
        if (again > markedUpTo && !signal.aborted) {
          markRead(again);
        }
      });
    };
    /** Marks read what arrived unread, once the reader has seen the newest. */
    const markSeen = (): void => {
      if (held.posts.some((p) => p.unread === true && p.id > markedUpTo)) {
        markRead();
      }
    };
    feed.onCaughtUp(markSeen);
    // An arrival while the tab was hidden at the newest moves no unseen count, so onCaughtUp never fires for it.
    document.addEventListener(
      "visibilitychange",
      () => {
        if (document.visibilityState === "visible" && frame?.scroll.unseen === 0) {
          markSeen();
        }
      },
      { signal },
    );

    const draw = async (): Promise<void> => {
      if (frame === null) {
        return;
      }
      composer?.settle(held.posts, readFrom);
      feed.draw(held.posts, {
        marker,
        path,
        pending: composer?.pending() ?? [],
        retry: (n) => composer?.retry(n),
      });
      if (held.next === undefined) {
        head.remove();
      } else if (!head.isConnected) {
        feed.list.before(head);
      }
      const any = (): boolean => held.posts.length > 0 || (composer?.pending().length ?? 0) > 0;
      if (any()) {
        empty?.remove();
        empty = null;
        feed.list.hidden = false;
        return;
      }
      const retention = await opts.retention;
      if (signal.aborted || any()) {
        return;
      }
      const words =
        retention === null
          ? "No messages."
          : `No messages in the last ${retentionWords(retention)}.`;
      feed.list.hidden = true;
      empty ??= el("div", { className: "cp-empty" }, el("strong"));
      if (empty.firstElementChild !== null) {
        empty.firstElementChild.textContent = words;
      }
      feed.list.before(empty);
    };

    /** The composer, or for two agents' conversation a way to write to either. */
    const reply = (t: Thread): HTMLElement => {
      const to = replyTarget(t);
      if (to !== null) {
        composer = conversationComposer({
          id,
          to,
          sends: opts.sends,
          signal,
          page: opts.page,
          onPending: () => void draw(),
          onSent: () => void reads.changed(),
        });
        return composer.form;
      }
      const sides = (t.agents ?? []).map((a) => `@${a}`);
      const ways = (t.agents ?? []).map((a) => {
        const write = el("button", { className: "cp-button", type: "button" }, `Message @${a}`);
        write.addEventListener("click", () => {
          opts.compose(a);
        });
        return write;
      });
      return el(
        "div",
        { className: "cp-stack", "data-gap": "tight" },
        el(
          "p",
          null,
          `Only ${new Intl.ListFormat("en", { type: "conjunction" }).format(sides)} write here.`,
        ),
        el("div", { className: "cp-cluster", "data-gap": "tight" }, ...ways),
      );
    };

    const build = (c: Conversation): void => {
      path = c.thread.kind === "path" ? (c.thread.path ?? null) : null;
      hasContext = c.thread.kind !== "announce";
      const context = conversationContext(c, signal);
      const main = context?.main ?? body;
      body.replaceChildren(...(context === null ? [] : [context.root]));
      main.append(feed.list, feed.foot);
      if (!replying) {
        replying = true;
        feed.foot.append(reply(c.thread));
        composer?.shown();
      }
      frame = {
        main,
        update: (next) => context?.update(next),
        scroll: feed.start(),
      };
    };

    const setRefreshNotice = (failure: Failure | null): void => {
      refreshNotice?.remove();
      refreshNotice = null;
      if (failure === null || frame === null) {
        return;
      }
      refreshNotice = el(
        "div",
        { className: "cp-notice", "data-tone": "danger" },
        icon("circle-x"),
        el("p", null, `The conversation could not be refreshed. ${failureWords(failure)}`),
        retryButton(() => {
          void reads.retry();
        }),
      );
      frame.main.prepend(refreshNotice);
      announce("The conversation could not be refreshed.");
    };

    const end = (): boolean => {
      frame = null;
      ended = true;
      gone();
      return false;
    };

    const failed = (failure: Failure): void => {
      if (frame !== null) {
        setRefreshNotice(failure);
        return;
      }
      if (!titled) {
        heading.textContent = "The conversation could not be loaded.";
      }
      body.replaceChildren(
        el(
          "div",
          { className: "cp-notice", "data-tone": "danger" },
          icon("circle-x"),
          el("p", null, failureWords(failure)),
          retryButton(() => {
            void reads.retry();
          }),
        ),
      );
    };

    const loadFirst = async (asking: AbortSignal): Promise<Answer> => {
      const from = storedCount(opts.sends, id);
      const result = await getConversation({ id }, { signal: asking });
      if (!result.ok) {
        if (result.code === "aborted") {
          return null;
        }
        return result.code === "not_found" ? { draw: end } : { failure: result };
      }
      const c = result.data;
      return {
        draw: () => {
          title(c.thread);
          held = { posts: c.posts, next: c.next };
          readFrom = from;
          marker = c.posts.find((p) => p.unread === true)?.id ?? null;
          build(c);
          void draw();
          frame?.scroll.toNewest();
          // Hidden, the visibilitychange listener marks it once the tab is shown.
          if (document.visibilityState === "visible") {
            markRead();
          }
          return false;
        },
      };
    };

    /** Reads every page held again, so each post shown is the server's current one. */
    const walk = async (asking: AbortSignal): Promise<Answer> => {
      const before = held;
      const from = storedCount(opts.sends, id);
      const result = await getConversation({ id }, { signal: asking });
      if (!result.ok) {
        if (result.code === "aborted") {
          return null;
        }
        return result.code === "not_found" ? { draw: end } : { failure: result };
      }
      const c = result.data;
      const walked = await rewalk<Post, number>({
        held: { items: before.posts.toReversed(), next: before.next },
        fresh: { items: c.posts.toReversed(), next: c.next },
        more: async (cursor) => {
          const page = await getConversation({ id, before: cursor }, { signal: asking });
          return page.ok
            ? { ok: true, data: { items: page.data.posts.toReversed(), next: page.data.next } }
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
      const posts = walked.window.items.toReversed();
      const next = walked.window.next;
      return {
        draw: () => {
          if (held !== before) {
            return true;
          }
          setRefreshNotice(null);
          title(c.thread);
          frame?.update(c);
          const newest = held.posts.at(-1)?.id ?? 0;
          held = { posts, next };
          readFrom = from;
          void draw();
          const arrived = held.posts.filter((p) => p.id > newest && p.from_kind !== "user");
          if (arrived.length > 0) {
            // The feed counts arrivals on its own observer: read it after that runs.
            setTimeout(() => {
              if (signal.aborted || frame?.scroll.unseen !== 0) {
                return;
              }
              announce(arrivalWords(arrived));
              if (document.visibilityState === "visible") {
                markSeen();
              }
            });
          }
          return false;
        },
      };
    };

    const reads = reader(signal, {
      holds: () => frame !== null,
      load: loadFirst,
      refresh: walk,
      skeleton: () => {
        feed.skeleton();
        body.replaceChildren(feed.list);
      },
      failed,
    });

    enhanceLoadMore(older, {
      list: feed.list,
      edge: "start",
      words: { failed: "Older messages could not be loaded." },
      signal,
      load: async (asking) => {
        const before = held.next;
        if (before === undefined) {
          return { items: [], more: false };
        }
        const result = await getConversation({ id, before }, { signal: asking });
        if (!result.ok) {
          throw new Error(failureWords(result));
        }
        if (held.next !== before) {
          return { items: [], more: held.next !== undefined };
        }
        const ids = new Set(held.posts.map((p) => p.id));
        const added = result.data.posts.filter((p) => !ids.has(p.id));
        held = { posts: [...added, ...held.posts], next: result.data.next };
        await draw();
        const items = added.flatMap((p) => feed.entry(p.id) ?? []);
        return { items, more: held.next !== undefined };
      },
    });

    opts.live.watch(
      {
        changed: (change) => {
          if (
            !ended &&
            (change.conversations.includes(id) || (change.agents && (hasContext || frame === null)))
          ) {
            void reads.changed();
          }
        },
        resync: (outer) => (ended ? Promise.resolve() : reads.resync(outer)),
      },
      signal,
    );
    void reads.load();
  };

  return {
    section,
    show(id, known) {
      if (id !== shown) {
        view?.abort();
        view = new AbortController();
        shown = id;
        titled = false;
        placeholder();
        body.replaceChildren();
        run(id, AbortSignal.any([opts.signal, view.signal]));
      }
      if (known !== undefined && !titled) {
        title(known);
      }
    },
    clear() {
      view?.abort();
      view = null;
      shown = null;
    },
    focus() {
      heading.focus();
    },
  };
}
