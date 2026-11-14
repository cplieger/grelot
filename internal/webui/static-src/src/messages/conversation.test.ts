import { delay, http, HttpResponse } from "msw";
import { describe, expect, it, onTestFinished } from "vitest";
import { userEvent } from "vitest/browser";

import { until } from "../__test-helpers__/until.js";

import { clockTime } from "../format.js";
import type { Agent, Conversation, Post, Receipts, Reservation } from "../wire/types.gen.js";
import {
  answer,
  at,
  button,
  conversationOf,
  emit,
  h,
  hour,
  pageOf,
  rested,
  row,
  start,
  useMessagesHarness,
  worker,
} from "./__test-helpers__/harness.js";

useMessagesHarness();

const minute = 60_000;

function post(id: number, over: Partial<Post> = {}): Post {
  return {
    id,
    thread: 1,
    from: "bravo",
    from_kind: "agent",
    subject: `s${String(id)}`,
    body: `b${String(id)}`,
    read: { read: 1, total: 2 },
    at_ms: at - (10 - id) * minute,
    ...over,
  };
}

function agent(name: string, over: Partial<Agent> = {}): Agent {
  return {
    name,
    description: "",
    state: "live",
    paths: [],
    follows: [],
    registered_ms: at,
    ...over,
  };
}

/** /api/conversations/{id} answers the conversations in turn, the last one repeating. */
function serve(
  id: number,
  ...answers: (Conversation | (() => Response | Promise<Response>))[]
): URLSearchParams[] {
  const asked: URLSearchParams[] = [];
  worker.use(
    http.get(`/api/conversations/${String(id)}`, ({ request }) => {
      asked.push(new URL(request.url).searchParams);
      const next = answers.length > 1 ? answers.shift() : answers[0];
      return typeof next === "function" ? next() : HttpResponse.json(next);
    }),
  );
  return asked;
}

const busy = (): Response =>
  HttpResponse.json({ error: "The store is busy.", code: "timeout" }, { status: 503 });

/** Counts the mark-read requests for conversation id. */
function reads(id: number): { n: number } {
  const count = { n: 0 };
  worker.use(
    http.post(`/api/conversations/${String(id)}/read`, () => {
      count.n++;
      return HttpResponse.json({ read: 1 });
    }),
  );
  return count;
}

/**
 * Answers conversation id's mark-reads with answers in turn, the last one
 * repeating, and returns each request's up_to.
 */
function readBounds(
  id: number,
  ...answers: (() => Response | Promise<Response>)[]
): (number | undefined)[] {
  const bounds: (number | undefined)[] = [];
  worker.use(
    http.post(`/api/conversations/${String(id)}/read`, async ({ request }) => {
      const body: unknown = await request.json();
      bounds.push((body as { up_to?: number }).up_to);
      const next = answers.length > 1 ? answers.shift() : answers[0];
      return next === undefined ? HttpResponse.json({ read: 1 }) : next();
    }),
  );
  return bounds;
}

/**
 * Counts the mark-read requests the page sends for conversation id, at the
 * moment it calls fetch: the client sends one synchronously from the callback
 * that decides it, so a check after that callback needs no wait.
 */
function readsSent(id: number): { n: number } {
  const count = { n: 0 };
  const real = globalThis.fetch;
  const path = `/api/conversations/${String(id)}/read`;
  globalThis.fetch = (input, init) => {
    if (init?.method === "POST" && String(input) === path) {
      count.n++;
    }
    return real(input, init);
  };
  onTestFinished(() => {
    globalThis.fetch = real;
  });
  return count;
}

const detail = (): HTMLElement => h.root.querySelector(".cp-detail") as HTMLElement;
const feed = (): HTMLElement => detail().querySelector("ol.cp-feed") as HTMLElement;
const entries = (): HTMLElement[] => [
  ...detail().querySelectorAll<HTMLElement>("li.cp-feed-entry:not([data-skeleton])"),
];
const subjects = (): string[] => entries().map((e) => e.querySelector("h3")?.textContent ?? "");
const politely = (): string =>
  document.body.querySelector('[aria-live="polite"]')?.textContent ?? "";

async function shown(n: number): Promise<void> {
  await until(() => {
    expect(entries().length).toBe(n);
  });
}

describe("the open conversation", () => {
  it("draws each message: its author, origin, time, subject, body and read summary", async () => {
    answer(pageOf([row(1)]));
    const day1 = new Date(2026, 10, 12, 18, 0).getTime();
    const day2 = new Date(2026, 10, 13, 9, 0).getTime();
    serve(
      1,
      conversationOf(1, {
        posts: [
          post(1, {
            subject: "Queue",
            body: "**Mine** until noon <img src=x onerror=alert(1)>",
            at_ms: day1,
          }),
          post(2, {
            from: "user",
            from_kind: "user",
            subject: "Merge order",
            origin: "/workspace/r1/web",
            read: { read: 0, total: 0 },
            at_ms: day2,
          }),
          post(3, { from: "alpha/helper", subject: "Done", at_ms: day2 + minute }),
        ],
      }),
    );
    start("/messages/1");
    await shown(3);
    const [theirs, own, delegate] = entries() as [HTMLElement, HTMLElement, HTMLElement];

    const author = theirs.querySelector("header a");
    expect(author?.textContent).toBe("@bravo");
    expect(author?.getAttribute("href")).toBe("/agents/bravo");
    expect(theirs.querySelector("header time")?.getAttribute("datetime")).toBe(
      new Date(day1).toISOString(),
    );
    expect(subjects()).toEqual(["Queue", "Merge order", "Done"]);
    expect(theirs.querySelector(".cp-prose strong")?.textContent).toBe("Mine");
    expect(theirs.querySelector(".cp-prose img")).toBeNull();
    expect(theirs.querySelector(".cp-prose")?.textContent).toContain(
      "<img src=x onerror=alert(1)>",
    );
    expect(theirs.querySelector("footer button")?.textContent).toBe("Read by 1 of 2");
    expect(theirs.hasAttribute("data-own")).toBe(false);

    expect(own.hasAttribute("data-own")).toBe(true);
    expect(own.querySelector("header a")?.textContent).toBe("You");
    expect(own.querySelector("header a")?.getAttribute("href")).toBe("/agents/user");
    expect(own.querySelector("header .cp-chip .cp-mono")?.textContent).toBe("web");
    expect(own.querySelector("footer")?.textContent).toBe("Reached no one.");

    expect(delegate.querySelector("header a")).toBeNull();
    expect(delegate.querySelector("header")?.firstElementChild?.textContent).toBe("@alpha/helper");

    expect([...feed().children].map((li) => li.className)).toEqual([
      "cp-feed-day",
      "cp-feed-entry",
      "cp-feed-day",
      "cp-feed-entry",
      "cp-feed-entry",
    ]);
    expect(
      [...feed().querySelectorAll(".cp-feed-day time")].map((t) => t.getAttribute("datetime")),
    ).toEqual(["2026-11-12", "2026-11-13"]);
    expect(feed().hasAttribute("aria-busy")).toBe(false);
  });

  it("marks where the unread messages start, and marks the conversation read once per open", async () => {
    answer(pageOf([row(11)]));
    const unread = [post(1), post(2, { unread: true }), post(3, { unread: true })];
    const read = unread.map((p) => ({ ...p, unread: false }));
    const asked = serve(
      11,
      conversationOf(11, { posts: unread }),
      conversationOf(11, { posts: read }),
    );
    const marked = reads(11);
    const router = start("/messages/11");
    await shown(3);
    const marker = feed().querySelector(".cp-since");
    expect(marker?.textContent).toBe("New since your last visit");
    expect(marker?.nextElementSibling).toBe(entries()[1]);
    await until(() => {
      expect(marked.n).toBe(1);
    });

    emit({ conversations: [11], agents: false });
    await until(() => {
      expect(asked.length).toBe(2);
    });
    await until(() => {
      expect(feed().querySelector(".cp-since")?.nextElementSibling).toBe(entries()[1]);
    });
    expect(marked.n).toBe(1);

    router.go({ screen: "messages", show: "all", conversation: null });
    router.go({ screen: "messages", show: "all", conversation: 11 });
    await until(() => {
      expect(marked.n).toBe(2);
    });
  });

  it("shows a path thread's reservations, its queue in order and its followers", async () => {
    answer(pageOf([row(1)]));
    const until = new Date(2026, 10, 13, 11, 30).getTime();
    const reservations: Reservation[] = [
      {
        id: 7,
        path: "/workspace/r1/web",
        holder: "alpha",
        state: "held",
        since_ms: at,
        expires_ms: until,
        reason: "fixing the feed",
      },
      {
        id: 8,
        path: "/workspace/r1",
        holder: "bravo",
        state: "queued",
        place: 1,
        since_ms: at,
        expires_ms: at + hour,
      },
      {
        id: 9,
        path: "/workspace/r1",
        holder: "charlie",
        state: "queued",
        place: 2,
        since_ms: at,
        expires_ms: at + hour,
      },
    ];
    serve(1, conversationOf(1, { posts: [post(1)], reservations, followers: ["alpha", "bravo"] }));
    start("/messages/1");
    await shown(1);

    const panel = detail().querySelector("details.cp-context-panel") as HTMLElement;
    expect(panel.parentElement?.classList.contains("cp-context")).toBe(true);
    expect(panel.querySelector("summary")?.textContent).toBe("Reservations, queue and followers");
    expect([...panel.querySelectorAll("h3")].map((x) => x.textContent)).toEqual([
      "Reservations",
      "Followers",
    ]);
    expect(
      [...panel.querySelectorAll(":scope > section:first-of-type li")].map((li) => li.textContent),
    ).toEqual([
      `@alpha holds /workspace/r1/web until ${clockTime(until)}: fixing the feed`,
      "@bravo, 1st in queue for /workspace/r1",
      "@charlie, 2nd in queue for /workspace/r1",
    ]);
    expect(panel.querySelector(":scope > section:first-of-type li a")?.getAttribute("href")).toBe(
      "/agents/alpha",
    );
    expect(
      [...panel.querySelectorAll(":scope > section:last-of-type a")].map((a) =>
        a.getAttribute("href"),
      ),
    ).toEqual(["/agents/alpha", "/agents/bravo"]);
    expect(feed().closest(".cp-context-main")?.parentElement).toBe(panel.parentElement);
  });

  it("says each empty group, and redraws only the group an agents change moved", async () => {
    answer(pageOf([row(1)]));
    serve(
      1,
      conversationOf(1, { posts: [post(1)] }),
      conversationOf(1, { posts: [post(1)], followers: ["charlie"] }),
    );
    start("/messages/1");
    await shown(1);
    const groups = (): HTMLElement[] => [
      ...detail().querySelectorAll<HTMLElement>("details.cp-context-panel section"),
    ];
    expect(groups().map((s) => s.querySelector(".cp-meta")?.textContent)).toEqual([
      "No reservations.",
      "No followers.",
    ]);
    const [reservationsGroup] = groups();

    emit({ conversations: [], agents: true });
    await until(() => {
      expect(groups()[1]?.querySelector("a")?.textContent).toBe("@charlie");
    });
    expect(groups()[0]).toBe(reservationsGroup);
  });

  it("shows the agent across a direct conversation, and a side that no longer exists", async () => {
    answer(
      pageOf([
        row(2, { kind: "direct", title: "@alpha and @user", agents: ["alpha", "user"] }),
        row(3, { kind: "direct", title: "@alpha and @bravo", agents: ["alpha", "bravo"] }),
      ]),
    );
    serve(
      2,
      conversationOf(2, {
        posts: [post(1)],
        cards: [
          agent("alpha", { description: "Builds the feed", host: "borgcube" }),
          agent("user", { description: "the user" }),
        ],
      }),
    );
    serve(
      3,
      conversationOf(3, { posts: [post(1)], cards: [agent("bravo", { state: "dormant" })] }),
    );
    const router = start("/messages/2");
    await shown(1);
    const panel = (): HTMLElement =>
      detail().querySelector("details.cp-context-panel") as HTMLElement;
    expect(panel().querySelector("summary")?.textContent).toBe("About @alpha");
    const sections = panel().querySelectorAll("section");
    expect(sections.length).toBe(1);
    const card = sections[0] as HTMLElement;
    expect(card.querySelector("h3 a")?.getAttribute("href")).toBe("/agents/alpha");
    expect(card.textContent).toContain("Builds the feed");
    expect(card.querySelector(".cp-state")?.textContent).toBe("Live");
    expect(card.querySelector(".cp-state")?.getAttribute("data-tone")).toBe("success");
    expect(card.textContent).toContain("Machine: borgcube");

    router.go({ screen: "messages", show: "all", conversation: 3 });
    await until(() => {
      expect(panel().querySelector("summary")?.textContent).toBe("About @alpha and @bravo");
    });
    expect([...panel().querySelectorAll("section")].map((s) => s.textContent)).toEqual([
      "@alphaNo longer registered.",
      "@bravoDormant",
    ]);
  });

  it("puts the announcement thread's feed straight in the detail, with nothing around it", async () => {
    answer(pageOf([row(1, { kind: "announce", title: "announce" })]));
    serve(1, conversationOf(1, { posts: [post(1)] }));
    start("/messages/1");
    await shown(1);
    expect(detail().querySelector(".cp-context")).toBeNull();
    expect(feed().parentElement?.classList.contains("cp-detail-body")).toBe(true);
    expect(feed().nextElementSibling?.classList.contains("cp-feed-foot")).toBe(true);
  });

  it("opens who read a message, says when that fails, and closes on Escape back to its button", async () => {
    answer(pageOf([row(1)]));
    serve(1, conversationOf(1, { posts: [post(5, { read: { read: 1, total: 4 } })] }));
    const receipts: Receipts = {
      subject: "s5",
      kind: "path",
      message: 5,
      posted_at_ms: at,
      registered_since: 0,
      recipients: [
        { name: "user", state: "read", read_at_ms: at },
        { name: "alpha", state: "unread" },
        { name: "old", state: "retired" },
        { name: "bravo", state: "dormant" },
      ],
    };
    const answers = [busy, () => HttpResponse.json(receipts)];
    worker.use(http.get("/api/messages/5/receipts", () => (answers.shift() ?? busy)()));
    start("/messages/1");
    await shown(1);
    const summary = button("Read by 1 of 4", detail());
    const panel = document.getElementById(
      summary.getAttribute("popovertarget") ?? "",
    ) as HTMLElement;

    await userEvent.click(summary);
    await until(() => {
      expect(panel.textContent).toContain("Who read it could not be loaded. The store is busy.");
    });
    expect(panel.matches(":popover-open")).toBe(true);
    const retry = button("Retry", panel);
    expect(document.activeElement).toBe(retry);

    retry.click();
    await until(() => {
      expect([...panel.querySelectorAll("h4")].map((x) => x.textContent)).toEqual([
        "Read",
        "Not read yet",
      ]);
    });
    const [readGroup, waitingGroup] = [...panel.querySelectorAll("section")];
    expect([...(readGroup?.querySelectorAll("li") ?? [])].map((li) => li.textContent)).toEqual([
      "You",
    ]);
    expect(readGroup?.querySelector("a")?.getAttribute("href")).toBe("/agents/user");
    expect([...(waitingGroup?.querySelectorAll("li") ?? [])].map((li) => li.textContent)).toEqual([
      "@alpha",
      "@old, retired",
      "@bravo, dormant",
    ]);
    expect(waitingGroup?.querySelector(":scope li:nth-child(2) a")).toBeNull();
    expect(document.activeElement).toBe(panel.querySelector("h4"));

    await userEvent.keyboard("{Escape}");
    expect(panel.matches(":popover-open")).toBe(false);
    expect(document.activeElement).toBe(summary);
  });

  it("adds a message the stream reports, says who sent it and marks it read at the newest", async () => {
    answer(pageOf([row(14)]));
    const asked = serve(
      14,
      conversationOf(14, { posts: [post(1)] }),
      conversationOf(14, { posts: [post(1), post(2, { from: "alpha", unread: true })] }),
    );
    const marked = reads(14);
    start("/messages/14");
    await shown(1);
    await until(() => {
      expect(marked.n).toBe(1);
    });
    const first = entries()[0];

    emit({ conversations: [9], agents: false });
    emit({ conversations: [14], agents: false });
    await shown(2);
    expect(asked.length).toBe(2);
    expect(entries()[0]).toBe(first);
    await until(() => {
      expect(politely()).toBe("New message from @alpha");
    });
    await until(() => {
      expect(marked.n).toBe(2);
    });
  });

  it("leaves an arrival behind New messages while the reader is scrolled up, and marks it read once they reach it", async () => {
    answer(pageOf([row(12)]));
    const many = Array.from({ length: 30 }, (_, i) => post(i + 1, { body: "a\n\nb\n\nc" }));
    serve(
      12,
      conversationOf(12, { posts: many }),
      conversationOf(12, { posts: [...many, post(31, { from: "alpha", unread: true })] }),
    );
    const marked = reads(12);
    const sent = readsSent(12);
    start("/messages/12");
    await shown(30);
    await until(() => {
      expect(marked.n).toBe(1);
    });
    let scroller: HTMLElement | null = feed();
    while (scroller !== null && scroller.scrollHeight <= scroller.clientHeight) {
      scroller = scroller.parentElement;
    }
    expect(scroller).not.toBeNull();
    const scrolled = new Promise((done) => {
      document.addEventListener("scroll", done, { once: true, capture: true });
    });
    scroller?.scrollTo({ top: 0 });
    await scrolled;

    emit({ conversations: [12], agents: false });
    await shown(31);
    const jump = detail().querySelector<HTMLButtonElement>(".cp-feed-jump");
    await until(() => {
      expect(jump?.hidden).toBe(false);
    });
    // The arrival's mark-read decision runs on a timer queued before this one.
    await new Promise((done) => setTimeout(done, 0));
    expect(sent.n).toBe(1);
    expect(politely()).not.toBe("New message from @alpha");

    jump?.click();
    await until(() => {
      expect(marked.n).toBe(2);
    });
  });

  it("marks nothing again that an earlier mark-read covered", async () => {
    answer(pageOf([row(13)]));
    serve(
      13,
      conversationOf(13, { posts: [post(1, { unread: true })] }),
      conversationOf(13, { posts: [post(1, { unread: true }), post(2, { from: "alpha" })] }),
    );
    const marked = reads(13);
    const sent = readsSent(13);
    start("/messages/13");
    await shown(1);
    await until(() => {
      expect(marked.n).toBe(1);
    });

    emit({ conversations: [13], agents: false });
    await until(() => {
      expect(politely()).toBe("New message from @alpha");
    });
    expect(sent.n).toBe(1);
  });

  it("marks read only up to the newest message shown", async () => {
    answer(pageOf([row(15)]));
    serve(
      15,
      conversationOf(15, { posts: [post(1, { unread: true }), post(4, { unread: true })] }),
    );
    const bounds = readBounds(15);
    start("/messages/15");
    await shown(2);
    await until(() => {
      expect(bounds).toEqual([4]);
    });
  });

  it("marks an arrival read once the mark-read in flight when it landed settles", async () => {
    answer(pageOf([row(16)]));
    serve(
      16,
      conversationOf(16, { posts: [post(1, { unread: true })] }),
      conversationOf(16, {
        posts: [post(1, { unread: true }), post(2, { from: "alpha", unread: true })],
      }),
    );
    const held = Promise.withResolvers<undefined>();
    const bounds = readBounds(
      16,
      async () => {
        await held.promise;
        return HttpResponse.json({ read: 1 });
      },
      () => HttpResponse.json({ read: 1 }),
    );
    start("/messages/16");
    await shown(1);
    await until(() => {
      expect(bounds).toEqual([1]);
    });

    emit({ conversations: [16], agents: false });
    await until(() => {
      expect(politely()).toBe("New message from @alpha");
    });
    held.resolve(undefined);
    await until(() => {
      expect(bounds).toEqual([1, 2]);
    });
  });

  it("tries a failed mark-read again the next time the reader reaches the newest", async () => {
    answer(pageOf([row(17)]));
    serve(
      17,
      conversationOf(17, { posts: [post(1, { unread: true })] }),
      conversationOf(17, { posts: [post(1, { unread: true }), post(2, { from: "alpha" })] }),
    );
    const bounds = readBounds(17, busy, () => HttpResponse.json({ read: 1 }));
    start("/messages/17");
    await shown(1);
    await until(() => {
      expect(bounds).toEqual([1]);
    });

    emit({ conversations: [17], agents: false });
    await until(() => {
      expect(bounds).toEqual([1, 2]);
    });
  });

  it("marks an arrival read once the mark-read in flight when it landed fails", async () => {
    answer(pageOf([row(19)]));
    serve(
      19,
      conversationOf(19, { posts: [post(1, { unread: true })] }),
      conversationOf(19, {
        posts: [post(1, { unread: true }), post(2, { from: "alpha", unread: true })],
      }),
    );
    const held = Promise.withResolvers<undefined>();
    const bounds = readBounds(
      19,
      async () => {
        await held.promise;
        return busy();
      },
      () => HttpResponse.json({ read: 1 }),
    );
    start("/messages/19");
    await shown(1);
    await until(() => {
      expect(bounds).toEqual([1]);
    });

    emit({ conversations: [19], agents: false });
    await until(() => {
      expect(politely()).toBe("New message from @alpha");
    });
    held.resolve(undefined);
    await until(() => {
      expect(bounds).toEqual([1, 2]);
    });
  });

  it.each([
    { id: 22, settles: "fails", first: busy },
    { id: 23, settles: "succeeds", first: () => HttpResponse.json({ read: 1 }) },
  ])(
    "marks only the arrival seen while a mark-read was in flight when it $settles, not one that came after",
    async ({ id, first }) => {
      let state: DocumentVisibilityState = "visible";
      Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
      onTestFinished(() => {
        Reflect.deleteProperty(document, "visibilityState");
      });
      answer(pageOf([row(id)]));
      const two = [post(1, { unread: true }), post(2, { from: "alpha", unread: true })];
      serve(
        id,
        conversationOf(id, { posts: [post(1, { unread: true })] }),
        conversationOf(id, { posts: two }),
        conversationOf(id, { posts: [...two, post(3, { from: "charlie", unread: true })] }),
      );
      const held = Promise.withResolvers<undefined>();
      const bounds = readBounds(
        id,
        async () => {
          await held.promise;
          return first();
        },
        () => HttpResponse.json({ read: 1 }),
      );
      start(`/messages/${String(id)}`);
      await shown(1);
      await until(() => {
        expect(bounds).toEqual([1]);
      });

      emit({ conversations: [id], agents: false });
      await until(() => {
        expect(politely()).toBe("New message from @alpha");
      });
      state = "hidden";
      emit({ conversations: [id], agents: false });
      await until(() => {
        expect(politely()).toBe("New message from @charlie");
      });
      held.resolve(undefined);
      await until(() => {
        expect(bounds).toHaveLength(2);
      });
      expect(bounds).toEqual([1, 2]);

      state = "visible";
      document.dispatchEvent(new Event("visibilitychange"));
      await until(() => {
        expect(bounds).toEqual([1, 2, 3]);
      });
    },
  );

  it("marks an arrival read that landed while the tab was hidden, once the tab is shown", async () => {
    let state: DocumentVisibilityState = "visible";
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
    onTestFinished(() => {
      Reflect.deleteProperty(document, "visibilityState");
    });
    answer(pageOf([row(20)]));
    serve(
      20,
      conversationOf(20, { posts: [post(1)] }),
      conversationOf(20, { posts: [post(1), post(2, { from: "alpha", unread: true })] }),
    );
    const bounds = readBounds(20);
    const sent = readsSent(20);
    start("/messages/20");
    await shown(1);
    await until(() => {
      expect(bounds).toEqual([1]);
    });

    state = "hidden";
    emit({ conversations: [20], agents: false });
    await until(() => {
      expect(politely()).toBe("New message from @alpha");
    });
    expect(sent.n).toBe(1);

    state = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
    await until(() => {
      expect(bounds).toEqual([1, 2]);
    });
  });

  it("leaves an arrival behind New messages unread when the tab is shown again, and marks it once the reader reaches it", async () => {
    let state: DocumentVisibilityState = "visible";
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
    onTestFinished(() => {
      Reflect.deleteProperty(document, "visibilityState");
    });
    answer(pageOf([row(21)]));
    const many = Array.from({ length: 30 }, (_, i) => post(i + 1, { body: "a\n\nb\n\nc" }));
    serve(
      21,
      conversationOf(21, { posts: many }),
      conversationOf(21, { posts: [...many, post(31, { from: "alpha", unread: true })] }),
    );
    const bounds = readBounds(21);
    const sent = readsSent(21);
    start("/messages/21");
    await shown(30);
    await until(() => {
      expect(bounds).toEqual([30]);
    });
    let scroller: HTMLElement | null = feed();
    while (scroller !== null && scroller.scrollHeight <= scroller.clientHeight) {
      scroller = scroller.parentElement;
    }
    expect(scroller).not.toBeNull();
    const scrolled = new Promise((done) => {
      document.addEventListener("scroll", done, { once: true, capture: true });
    });
    scroller?.scrollTo({ top: 0 });
    await scrolled;

    state = "hidden";
    emit({ conversations: [21], agents: false });
    await shown(31);
    const jump = detail().querySelector<HTMLButtonElement>(".cp-feed-jump");
    await until(() => {
      expect(jump?.hidden).toBe(false);
    });
    state = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
    expect(sent.n).toBe(1);

    jump?.click();
    await until(() => {
      expect(bounds).toEqual([30, 31]);
    });
  });

  it("fetches a change that lands while the conversation first loads, once it is shown", async () => {
    answer(pageOf([row(18)]));
    const held = Promise.withResolvers<undefined>();
    const asked = serve(
      18,
      async () => {
        await held.promise;
        return HttpResponse.json(conversationOf(18, { posts: [post(1)] }));
      },
      conversationOf(18, { posts: [post(1), post(2)] }),
    );
    start("/messages/18");
    await until(() => {
      expect(asked.length).toBe(1);
    });
    emit({ conversations: [18], agents: false });
    held.resolve(undefined);
    await shown(2);
    expect(subjects()).toEqual(["s1", "s2"]);
  });

  it("a resync fetches the open conversation again before it settles", async () => {
    answer(pageOf([row(1)]));
    serve(
      1,
      conversationOf(1, { posts: [post(1)] }),
      conversationOf(1, { posts: [post(1), post(2)] }),
    );
    start("/messages/1");
    await shown(1);
    await Promise.all(h.watchers.map((w) => w.resync?.(new AbortController().signal)));
    expect(subjects()).toEqual(["s1", "s2"]);
  });

  it("Show older adds the page before the oldest message, then leaves for its first message", async () => {
    answer(pageOf([row(1)]));
    const asked: URLSearchParams[] = [];
    worker.use(
      http.get("/api/conversations/1", ({ request }) => {
        const q = new URL(request.url).searchParams;
        asked.push(q);
        return HttpResponse.json(
          q.get("before") === "3"
            ? conversationOf(1, { posts: [post(1), post(2)] })
            : conversationOf(1, { posts: [post(3), post(4)], next: 3 }),
        );
      }),
    );
    start("/messages/1");
    await shown(2);
    const older = await rested("Show older", detail());
    expect(older.closest(".cp-feed-head")?.nextElementSibling).toBe(feed());

    older.focus();
    older.click();
    await shown(4);
    expect(subjects()).toEqual(["s1", "s2", "s3", "s4"]);
    expect(asked.at(-1)?.get("before")).toBe("3");
    await until(() => {
      expect(older.isConnected).toBe(false);
    });
    expect(document.activeElement).toBe(entries()[0]);
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "2 older messages",
      );
    });
  });

  it("a refresh keeps the older pages shown, updates read counts in place, and Show older goes on", async () => {
    answer(pageOf([row(1)]));
    const asked: URLSearchParams[] = [];
    const newest = [
      conversationOf(1, { posts: [post(5), post(6)], next: 5 }),
      conversationOf(1, { posts: [post(5), post(6, { read: { read: 2, total: 2 } })], next: 5 }),
    ];
    worker.use(
      http.get("/api/conversations/1", ({ request }) => {
        const q = new URL(request.url).searchParams;
        asked.push(q);
        switch (q.get("before")) {
          case "5":
            return HttpResponse.json(conversationOf(1, { posts: [post(3), post(4)], next: 3 }));
          case "3":
            return HttpResponse.json(conversationOf(1, { posts: [post(1), post(2)] }));
          default:
            return HttpResponse.json(newest.length > 1 ? newest.shift() : newest[0]);
        }
      }),
    );
    start("/messages/1");
    await shown(2);
    (await rested("Show older", detail())).click();
    await shown(4);
    const summary = button("Read by 1 of 2", entries()[3]);

    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(entries()[3]?.querySelector("footer button")?.textContent).toBe("Read by 2 of 2");
    });
    expect(entries()[3]?.querySelector("footer button")).toBe(summary);
    expect(subjects()).toEqual(["s3", "s4", "s5", "s6"]);

    (await rested("Show older", detail())).click();
    await shown(6);
    expect(asked.at(-1)?.get("before")).toBe("3");
  });

  it("a refresh reads the older pages again: an expired message leaves, a changed one is redrawn in place", async () => {
    answer(pageOf([row(1)]));
    let changed = false;
    const asked: URLSearchParams[] = [];
    worker.use(
      http.get("/api/conversations/1", ({ request }) => {
        const q = new URL(request.url).searchParams;
        asked.push(q);
        switch (q.get("before")) {
          case "5":
            return HttpResponse.json(
              changed
                ? conversationOf(1, {
                    posts: [post(2), post(3, { read: { read: 2, total: 2 } })],
                    next: 2,
                  })
                : conversationOf(1, { posts: [post(3), post(4)], next: 3 }),
            );
          case "3":
            return HttpResponse.json(conversationOf(1, { posts: [post(1), post(2)] }));
          default:
            return HttpResponse.json(conversationOf(1, { posts: [post(5), post(6)], next: 5 }));
        }
      }),
    );
    start("/messages/1");
    await shown(2);
    (await rested("Show older", detail())).click();
    await shown(4);
    const third = entries()[0];
    const summary = button("Read by 1 of 2", third);

    changed = true;
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(subjects()).toEqual(["s3", "s5", "s6"]);
    });
    expect(entries()[0]).toBe(third);
    expect(third?.querySelector("footer button")).toBe(summary);
    expect(summary.textContent).toBe("Read by 2 of 2");
    expect(asked.at(-1)?.get("before")).toBe("5");

    (await rested("Show older", detail())).click();
    await shown(5);
    expect(asked.at(-1)?.get("before")).toBe("3");
  });

  it("a Show older that lands while a refresh reads the older pages again stays shown", async () => {
    answer(pageOf([row(1)]));
    let refreshing = false;
    let oldest = 0;
    const walking = Promise.withResolvers<undefined>();
    const asked: URLSearchParams[] = [];
    worker.use(
      http.get("/api/conversations/1", async ({ request }) => {
        const q = new URL(request.url).searchParams;
        asked.push(q);
        switch (q.get("before")) {
          case "5":
            if (refreshing) {
              refreshing = false;
              await walking.promise;
            }
            return HttpResponse.json(conversationOf(1, { posts: [post(3), post(4)], next: 3 }));
          case "3":
            oldest += 1;
            return HttpResponse.json(
              conversationOf(1, {
                posts: [post(1, oldest > 1 ? { read: { read: 2, total: 2 } } : {}), post(2)],
              }),
            );
          default:
            return HttpResponse.json(conversationOf(1, { posts: [post(5), post(6)], next: 5 }));
        }
      }),
    );
    start("/messages/1");
    await shown(2);
    (await rested("Show older", detail())).click();
    await shown(4);

    refreshing = true;
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(asked.filter((q) => q.get("before") === "5").length).toBe(2);
    });
    (await rested("Show older", detail())).click();
    await shown(6);
    walking.resolve(undefined);
    await until(() => {
      expect(asked.filter((q) => q.get("before") === "3").length).toBe(2);
    });
    await until(() => {
      expect(subjects()).toEqual(["s1", "s2", "s3", "s4", "s5", "s6"]);
      expect(entries()[0]?.querySelector("footer button")?.textContent).toBe("Read by 2 of 2");
    });
  });

  it("keeps the older pages and the context under the refresh notice when reading one of them again fails", async () => {
    answer(pageOf([row(1)]));
    let failing = false;
    worker.use(
      http.get("/api/conversations/1", ({ request }) => {
        switch (new URL(request.url).searchParams.get("before")) {
          case "5":
            return failing
              ? busy()
              : HttpResponse.json(conversationOf(1, { posts: [post(3), post(4)], next: 3 }));
          default:
            return HttpResponse.json(
              conversationOf(1, {
                posts: [
                  post(5),
                  post(6, { read: { read: 2, total: 2 } }),
                  ...(failing ? [post(7)] : []),
                ],
                next: 5,
                followers: failing ? ["charlie"] : [],
              }),
            );
        }
      }),
    );
    start("/messages/1");
    await shown(2);
    (await rested("Show older", detail())).click();
    await shown(4);
    const drawn = entries();

    failing = true;
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(detail().querySelector('.cp-notice[data-tone="danger"]')?.textContent).toBe(
        "The conversation could not be refreshed. The store is busy.Retry",
      );
    });
    expect(subjects()).toEqual(["s3", "s4", "s5", "s6"]);
    entries().forEach((e, i) => {
      expect(e).toBe(drawn[i]);
    });
    expect((await rested("Show older", detail())).isConnected).toBe(true);
    const followers = detail().querySelectorAll("details.cp-context-panel section")[1];
    expect(followers?.querySelector(".cp-meta")?.textContent).toBe("No followers.");
  });

  it("draws a refresh that finishes while more changes arrive, then reads once more for them", async () => {
    answer(pageOf([row(1)]));
    let newest = 6;
    let refreshing = false;
    let newestPages = 0;
    const gates: PromiseWithResolvers<undefined>[] = [];
    worker.use(
      http.get("/api/conversations/1", async ({ request }) => {
        switch (new URL(request.url).searchParams.get("before")) {
          case "5":
            if (refreshing) {
              const gate = Promise.withResolvers<undefined>();
              gates.push(gate);
              await gate.promise;
            }
            return HttpResponse.json(conversationOf(1, { posts: [post(3), post(4)] }));
          default: {
            newestPages += 1;
            const posts = Array.from({ length: newest - 4 }, (_, i) => post(5 + i));
            return HttpResponse.json(conversationOf(1, { posts, next: 5 }));
          }
        }
      }),
    );
    start("/messages/1");
    await shown(2);
    (await rested("Show older", detail())).click();
    await shown(4);

    newest = 7;
    refreshing = true;
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(gates.length).toBe(1);
    });
    newest = 8;
    emit({ conversations: [1], agents: false });
    emit({ conversations: [1], agents: false });
    gates[0]?.resolve(undefined);
    await until(() => {
      expect(subjects().slice(0, 5)).toEqual(["s3", "s4", "s5", "s6", "s7"]);
    });
    await until(() => {
      expect(gates.length).toBe(2);
    });
    gates[1]?.resolve(undefined);
    await until(() => {
      expect(subjects()).toEqual(["s3", "s4", "s5", "s6", "s7", "s8"]);
    });

    newest = 9;
    refreshing = false;
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(subjects()).toEqual(["s3", "s4", "s5", "s6", "s7", "s8", "s9"]);
    });
    expect(newestPages).toBe(4);
  });

  it("marks nothing read while the tab is hidden when the conversation loads, and marks it once shown", async () => {
    let state: DocumentVisibilityState = "visible";
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => state });
    onTestFinished(() => {
      Reflect.deleteProperty(document, "visibilityState");
    });
    answer(pageOf([row(24)]));
    const held = Promise.withResolvers<undefined>();
    const asked = serve(24, async () => {
      await held.promise;
      return HttpResponse.json(
        conversationOf(24, { posts: [post(1, { unread: true }), post(2, { unread: true })] }),
      );
    });
    const bounds = readBounds(24);
    const sent = readsSent(24);
    start("/messages/24");
    await until(() => {
      expect(asked.length).toBe(1);
    });
    state = "hidden";
    held.resolve(undefined);
    await shown(2);
    expect(sent.n).toBe(0);

    state = "visible";
    document.dispatchEvent(new Event("visibilitychange"));
    await until(() => {
      expect(bounds).toEqual([2]);
    });
  });

  it("says how far back history goes when the conversation holds no message", async () => {
    answer(pageOf([row(1)]));
    serve(1, conversationOf(1));
    start("/messages/1");
    await until(() => {
      expect(detail().querySelector(".cp-empty")?.textContent).toBe(
        "No messages in the last 72 hours.",
      );
    });
    expect(feed().hidden).toBe(true);
  });

  it("draws loading entries while a slow conversation loads", async () => {
    answer(pageOf([row(1)]));
    serve(1, async () => {
      await delay(900);
      return HttpResponse.json(conversationOf(1, { posts: [post(1)] }));
    });
    start("/messages/1");
    await until(() => {
      expect(detail().querySelectorAll("li.cp-feed-entry[data-skeleton]").length).toBe(3);
    });
    expect(feed().getAttribute("aria-busy")).toBe("true");
    await shown(1);
    expect(detail().querySelector("[data-skeleton]")).toBeNull();
    expect(feed().hasAttribute("aria-busy")).toBe(false);
  });

  it("shows a failed load in a notice with the server's words, and Retry loads it", async () => {
    answer(pageOf([row(1)]));
    serve(1, busy, conversationOf(1, { posts: [post(1)] }));
    start("/messages/1");
    await until(() => {
      expect(detail().querySelector('.cp-notice[data-tone="danger"]')?.textContent).toContain(
        "The store is busy.",
      );
    });
    expect(detail().querySelector("h2")?.textContent).toBe("/workspace/r1");
    button("Retry", detail()).click();
    await shown(1);
    expect(detail().querySelector(".cp-notice:not([hidden])")).toBeNull();
  });

  it("a Retry pressed twice on a failed open sends one request, drawn and marked read once", async () => {
    answer(pageOf([row(31)]));
    const held = Promise.withResolvers<undefined>();
    const asked = serve(31, busy, async () => {
      await held.promise;
      return HttpResponse.json(conversationOf(31, { posts: [post(1, { unread: true })] }));
    });
    const marked = reads(31);
    start("/messages/31");
    const retry = await until(() => button("Retry", detail()));
    retry.click();
    retry.click();
    await until(() => {
      expect(asked.length).toBe(2);
    });
    held.resolve(undefined);
    await shown(1);
    await until(() => {
      expect(marked.n).toBe(1);
    });
    await delay(50);
    expect(asked.length).toBe(2);
    expect(marked.n).toBe(1);
    expect(detail().querySelector(".cp-notice:not([hidden])")).toBeNull();
  });

  it("draws a resync's answer over the refresh it replaced, whose late failure shows nothing", async () => {
    answer(pageOf([row(1)]));
    const held = Promise.withResolvers<undefined>();
    const asked = serve(
      1,
      conversationOf(1, { posts: [post(1)] }),
      async () => {
        await held.promise;
        return busy();
      },
      conversationOf(1, { posts: [post(1), post(2)] }),
    );
    start("/messages/1");
    await shown(1);
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(asked.length).toBe(2);
    });
    await Promise.all(h.watchers.map((w) => w.resync?.(new AbortController().signal)));
    expect(subjects()).toEqual(["s1", "s2"]);
    held.resolve(undefined);
    await delay(50);
    expect(subjects()).toEqual(["s1", "s2"]);
    expect(detail().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
  });

  it("loads a failed open again when the stream reports a change to it", async () => {
    answer(pageOf([row(1)]));
    const asked = serve(1, busy, conversationOf(1, { posts: [post(1)] }));
    start("/messages/1");
    await until(() => button("Retry", detail()));
    emit({ conversations: [1], agents: false });
    await shown(1);
    expect(asked.length).toBe(2);
    expect(detail().querySelector(".cp-notice:not([hidden])")).toBeNull();
  });

  it("loads a failed open again on a resync, before the resync settles", async () => {
    answer(pageOf([row(1)]));
    const asked = serve(1, busy, conversationOf(1, { posts: [post(1)] }));
    start("/messages/1");
    await until(() => button("Retry", detail()));
    await Promise.all(h.watchers.map((w) => w.resync?.(new AbortController().signal)));
    expect(subjects()).toEqual(["s1"]);
    expect(asked.length).toBe(2);
    expect(detail().querySelector(".cp-notice:not([hidden])")).toBeNull();
  });

  it("keeps the messages when a refresh fails, says so, and Retry refreshes", async () => {
    answer(pageOf([row(1)]));
    serve(
      1,
      conversationOf(1, { posts: [post(1)] }),
      busy,
      conversationOf(1, { posts: [post(1), post(2)] }),
    );
    start("/messages/1");
    await shown(1);
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(detail().querySelector('.cp-notice[data-tone="danger"]')?.textContent).toBe(
        "The conversation could not be refreshed. The store is busy.Retry",
      );
    });
    expect(subjects()).toEqual(["s1"]);
    button("Retry", detail().querySelector(".cp-notice") as HTMLElement).click();
    await shown(2);
    expect(detail().querySelector(".cp-notice:not([hidden])")).toBeNull();
  });

  it("says the conversation is gone when a refresh finds it expired", async () => {
    answer(pageOf([row(1)]));
    serve(1, conversationOf(1, { posts: [post(1)] }), () =>
      HttpResponse.json({ error: "no conversation 1", code: "not_found" }, { status: 404 }),
    );
    start("/messages/1");
    await shown(1);
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(detail().querySelector("h2")?.textContent).toBe("This conversation is gone.");
    });
    expect(entries()).toEqual([]);
  });
});

describe("agent-written text under a right-to-left page", () => {
  const direction = (n: Element | null | undefined): string =>
    n === null || n === undefined ? "missing" : getComputedStyle(n).direction;

  it("keeps a retired recipient's name in its own direction", async () => {
    document.documentElement.dir = "rtl";
    try {
      answer(pageOf([row(1)]));
      serve(1, conversationOf(1, { posts: [post(5, { read: { read: 0, total: 1 } })] }));
      const receipts: Receipts = {
        subject: "s5",
        kind: "path",
        message: 5,
        posted_at_ms: at,
        registered_since: 0,
        recipients: [{ name: "old", state: "retired" }],
      };
      worker.use(http.get("/api/messages/5/receipts", () => HttpResponse.json(receipts)));
      start("/messages/1");
      await shown(1);
      const summary = button("Read by 0 of 1", detail());
      const panel = document.getElementById(
        summary.getAttribute("popovertarget") ?? "",
      ) as HTMLElement;
      await userEvent.click(summary);
      await until(() => {
        expect(panel.querySelector("li")?.textContent).toBe("@old, retired");
      });
      expect(direction(panel.querySelector("li bdi"))).toBe("ltr");
    } finally {
      document.documentElement.removeAttribute("dir");
    }
  });

  it("keeps each name, path, subject, body and reason in its own direction", async () => {
    document.documentElement.dir = "rtl";
    try {
      answer(pageOf([row(1)]));
      const reservations: Reservation[] = [
        {
          id: 7,
          path: "/workspace/r1/web",
          holder: "alpha",
          state: "held",
          since_ms: at,
          expires_ms: at + hour,
          reason: "fixing the feed",
        },
      ];
      serve(
        1,
        conversationOf(1, {
          posts: [
            post(1, { subject: "Queue", body: "Mine until noon.", origin: "/workspace/r1/web" }),
          ],
          reservations,
          followers: ["alpha"],
        }),
      );
      start("/messages/1");
      await shown(1);
      const [entry] = entries();
      const strip = detail().querySelector(".cp-context-panel section li");
      const listRow = h.root.querySelector("a.cp-list-row");

      expect({
        heading: direction(detail().querySelector("#conversation-title")),
        author: direction(entry?.querySelector("header a")),
        origin: direction(entry?.querySelector("header .cp-chip .cp-mono")),
        subject: direction(entry?.querySelector("h3")),
        body: direction(entry?.querySelector(".cp-prose")),
        holder: direction(strip?.querySelector("a")),
        path: direction(strip?.querySelector("code")),
        reason: direction(strip?.querySelector("bdi")),
        rowTitle: direction(listRow?.querySelector(".cp-cell-stack > span")),
        rowPreview: direction(listRow?.querySelector(".cp-cell-stack > small")),
      }).toEqual({
        heading: "ltr",
        author: "ltr",
        origin: "ltr",
        subject: "ltr",
        body: "ltr",
        holder: "ltr",
        path: "ltr",
        reason: "ltr",
        rowTitle: "ltr",
        rowPreview: "ltr",
      });
      expect(direction(detail())).toBe("rtl");
    } finally {
      document.documentElement.removeAttribute("dir");
    }
  });
});
