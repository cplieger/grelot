import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { userEvent } from "vitest/browser";

import { until } from "../__test-helpers__/until.js";
import type { Conversation, Post } from "../wire/types.gen.js";
import {
  answer,
  at,
  button,
  conversationOf,
  emit,
  h,
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

/** /api/conversations/{id} answers the conversations in turn, the last one repeating. */
function serve(id: number, ...answers: Conversation[]): void {
  worker.use(
    http.get(`/api/conversations/${String(id)}`, () => {
      const next = answers.length > 1 ? answers.shift() : answers[0];
      return HttpResponse.json(next);
    }),
  );
}

type Answer = () => Response | Promise<Response>;

/** POST /api/posts answers in turn, the last one repeating; what each request sent. */
function posts(...answers: Answer[]): { bodies: unknown[]; keys: (string | null)[] } {
  const seen = { bodies: [] as unknown[], keys: [] as (string | null)[] };
  worker.use(
    http.post("/api/posts", async ({ request }) => {
      const body: unknown = await request.json();
      seen.keys.push(request.headers.get("Idempotency-Key"));
      seen.bodies.push(body);
      const next = answers.length > 1 ? answers.shift() : answers[0];
      return next === undefined ? HttpResponse.json({}) : next();
    }),
  );
  return seen;
}

const stored =
  (message: number, thread = 1): Answer =>
  () =>
    HttpResponse.json({ message, thread, delivered: 2 });

const storeFull: Answer = () =>
  HttpResponse.json(
    { error: "The store is full: new writes stop at 90 % of 512 MiB.", code: "store_full" },
    { status: 507 },
  );

/** POST /api/posts waits for release, then answers then, and answers each later send after. */
function heldPosts(
  then: Answer,
  after: Answer = stored(5),
): { seen: ReturnType<typeof posts>; release: () => void } {
  let release = (): void => undefined;
  const held = new Promise<void>((r) => {
    release = r;
  });
  const seen = posts(async () => {
    await held;
    return then();
  }, after);
  return { seen, release };
}

const detail = (): HTMLElement => h.root.querySelector(".cp-detail") as HTMLElement;
const entries = (): HTMLElement[] => [
  ...detail().querySelectorAll<HTMLElement>("li.cp-feed-entry:not([data-skeleton])"),
];
const subject = (): HTMLInputElement =>
  detail().querySelector("#compose-subject") as HTMLInputElement;
const message = (): HTMLTextAreaElement =>
  detail().querySelector("#compose-body") as HTMLTextAreaElement;
const errorOf = (field: HTMLElement): string =>
  field.closest(".cp-field")?.querySelector(".cp-field-error:not([hidden])")?.textContent ?? "";

async function shown(n: number): Promise<void> {
  await until(() => {
    expect(entries().length).toBe(n);
  });
}

async function write(s: string, b: string): Promise<void> {
  await userEvent.fill(subject(), s);
  await userEvent.fill(message(), b);
}

const mine = (id: number, subjectWords: string, body: string): Post =>
  post(id, { from: "user", from_kind: "user", subject: subjectWords, body });

/**
 * Conversation 1 holds post 4, and the user's "Merge order" as post 5 once a
 * POST stored it, however often it is fetched; the POSTs answer as heldPosts.
 */
function heldConversation(then: Answer): ReturnType<typeof heldPosts> {
  let kept = false;
  const keep =
    (a: Answer): Answer =>
    async () => {
      const r = await a();
      kept ||= r.ok;
      return r;
    };
  worker.use(
    http.get("/api/conversations/1", () =>
      HttpResponse.json(
        conversationOf(1, {
          posts: kept ? [post(4), mine(5, "Merge order", "Now.")] : [post(4)],
        }),
      ),
    ),
  );
  return heldPosts(keep(then), keep(stored(5)));
}

describe("the conversation's composer", () => {
  it("shows the post at once as sending, then as the stored post, and clears the fields", async () => {
    answer(pageOf([row(1)]));
    serve(
      1,
      conversationOf(1, { posts: [post(4)] }),
      conversationOf(1, { posts: [post(4), mine(5, "Merge order", "After **you**.")] }),
    );
    let release = (): void => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    const sent = posts(async () => {
      await held;
      return stored(5)();
    });
    start("/messages/1");
    await shown(1);
    await write("Merge order", "After **you**.");
    const send = button("Send", detail());
    send.focus();
    send.click();

    await shown(2);
    const own = entries()[1] as HTMLElement;
    expect(own.hasAttribute("data-own")).toBe(true);
    expect(own.getAttribute("data-state")).toBe("sending");
    expect(own.getAttribute("aria-busy")).toBe("true");
    expect(own.querySelector("footer")?.textContent).toBe("Sending…");
    expect(own.querySelector("header a")?.textContent).toBe("You");
    expect(own.querySelector("h3")?.textContent).toBe("Merge order");
    expect(own.querySelector(".cp-prose strong")?.textContent).toBe("you");
    expect(subject().value).toBe("Merge order");
    await until(() => {
      expect(sent.bodies.length).toBe(1);
    });
    expect(sent.bodies).toEqual([
      { to: { path: "/workspace/r1" }, subject: "Merge order", body: "After **you**." },
    ]);
    expect(sent.keys[0]).toMatch(/^"[0-9a-f]{32}"$/);

    release();
    await until(() => {
      expect(entries()[1]?.querySelector("footer")?.textContent).toBe("Read by 1 of 2");
    });
    expect(entries().length).toBe(2);
    expect(entries()[1]?.hasAttribute("data-state")).toBe(false);
    expect(entries()[1]?.hasAttribute("aria-busy")).toBe(false);
    expect(subject().value).toBe("");
    expect(message().value).toBe("");
    expect(subject().matches(":user-invalid")).toBe(false);
    expect(document.activeElement).toBe(message());
  });

  it("keeps what the person wrote while the post was on its way", async () => {
    answer(pageOf([row(1)]));
    serve(
      1,
      conversationOf(1, { posts: [post(4)] }),
      conversationOf(1, { posts: [post(4), mine(5, "Merge order", "Now.")] }),
    );
    let release = (): void => undefined;
    const held = new Promise<void>((r) => {
      release = r;
    });
    posts(async () => {
      await held;
      return stored(5)();
    });
    start("/messages/1");
    await shown(1);
    await write("Merge order", "Now.");
    button("Send", detail()).click();
    await shown(2);
    await userEvent.fill(message(), "And a follow-up.");
    release();
    await until(() => {
      expect(entries()[1]?.querySelector("footer")?.textContent).toBe("Read by 1 of 2");
    });
    expect(subject().value).toBe("Merge order");
    expect(message().value).toBe("And a follow-up.");
  });

  it("stops drawing a sent post once the conversation drew it, so it does not come back after it expires", async () => {
    answer(pageOf([row(1)]));
    serve(
      1,
      conversationOf(1, { posts: [post(4)] }),
      conversationOf(1, { posts: [post(4), mine(5, "Merge order", "Now.")] }),
      conversationOf(1, { posts: [] }),
    );
    posts(stored(5));
    start("/messages/1");
    await shown(1);
    await write("Merge order", "Now.");
    button("Send", detail()).click();
    await until(() => {
      expect(entries()[1]?.querySelector("footer")?.textContent).toBe("Read by 1 of 2");
    });

    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(detail().textContent).toContain("No messages in the last 72 hours.");
    });
    expect(entries()).toEqual([]);
  });

  it("posts to announce from Announcements and to the agent from a direct conversation", async () => {
    answer(
      pageOf([
        row(2, { kind: "announce", title: "announce" }),
        row(3, { kind: "direct", title: "@alpha and @user", agents: ["user", "alpha"] }),
      ]),
    );
    const sent = posts(stored(9, 2));
    const router = start("/messages/2");
    await until(() => {
      expect(subject()).not.toBeNull();
    });
    await write("Deploy", "At 3.");
    button("Send", detail()).click();
    await until(() => {
      expect(sent.bodies.length).toBe(1);
    });

    router.go({ screen: "messages", show: "all", conversation: 3 });
    await until(() => {
      expect(detail().querySelector("h2")?.textContent).toBe("You and @alpha");
    });
    await until(() => {
      expect(subject()).not.toBeNull();
    });
    await write("Hello", "");
    button("Send", detail()).click();
    await until(() => {
      expect(sent.bodies.length).toBe(2);
    });
    expect(sent.bodies).toEqual([
      { to: { announce: true }, subject: "Deploy", body: "At 3." },
      { to: { agent: "alpha" }, subject: "Hello", body: "" },
    ]);
  });

  it("marks a refused post Not sent, keeps the text with the server's words, and Retry sends it again under its key", async () => {
    answer(pageOf([row(1)]));
    serve(
      1,
      conversationOf(1, { posts: [post(4)] }),
      conversationOf(1, { posts: [post(4), mine(5, "Merge order", "Now.")] }),
    );
    const sent = posts(storeFull, stored(5));
    start("/messages/1");
    await shown(1);
    await write("Merge order", "Now.");
    button("Send", detail()).click();

    await until(() => {
      expect(entries()[1]?.getAttribute("data-state")).toBe("failed");
    });
    const failed = entries()[1] as HTMLElement;
    expect(failed.hasAttribute("aria-busy")).toBe(false);
    expect(failed.querySelector('footer .cp-state[data-tone="danger"]')?.textContent).toBe(
      "Not sent.",
    );
    expect(subject().value).toBe("Merge order");
    expect(message().value).toBe("Now.");
    expect(message().getAttribute("aria-invalid")).toBe("true");
    expect(errorOf(message())).toBe("The store is full: new writes stop at 90 % of 512 MiB.");
    expect(document.activeElement).toBe(message());

    button("Retry", failed).click();
    await until(() => {
      expect(entries()[1]?.querySelector("footer")?.textContent).toBe("Read by 1 of 2");
    });
    expect(entries().length).toBe(2);
    expect(sent.keys.length).toBe(2);
    expect(sent.keys[1]).toBe(sent.keys[0]);
    expect(sent.bodies[1]).toEqual(sent.bodies[0]);
    expect(message().value).toBe("");
  });

  it("sends the text of a failed post again as that post, under its key", async () => {
    answer(pageOf([row(1)]));
    serve(1, conversationOf(1, { posts: [post(4)] }));
    const sent = posts(storeFull);
    start("/messages/1");
    await shown(1);
    await write("Merge order", "Now.");
    button("Send", detail()).click();
    await until(() => {
      expect(entries()[1]?.getAttribute("data-state")).toBe("failed");
    });
    (await rested("Send", detail())).click();
    await until(() => {
      expect(sent.keys.length).toBe(2);
    });
    expect(sent.keys[1]).toBe(sent.keys[0]);
    await until(() => {
      expect(entries()[1]?.getAttribute("data-state")).toBe("failed");
    });
    expect(entries().length).toBe(2);

    await userEvent.fill(message(), "Now, please.");
    (await rested("Send", detail())).click();
    await until(() => {
      expect(sent.keys.length).toBe(3);
    });
    expect(sent.keys[2]).not.toBe(sent.keys[0]);
    await shown(3);
  });

  it("puts a refusal of the subject under Subject", async () => {
    answer(pageOf([row(1)]));
    serve(1, conversationOf(1, { posts: [post(4)] }));
    posts(() =>
      HttpResponse.json(
        { error: "the subject must be 1 to 120 characters of text, on one line", code: "invalid" },
        { status: 400 },
      ),
    );
    start("/messages/1");
    await shown(1);
    await write("Merge order", "Now.");
    button("Send", detail()).click();
    await until(() => {
      expect(subject().getAttribute("aria-invalid")).toBe("true");
    });
    expect(errorOf(subject())).toBe("the subject must be 1 to 120 characters of text, on one line");
    expect(message().hasAttribute("aria-invalid")).toBe(false);
  });

  it("says what to fix before sending, and sends nothing", async () => {
    answer(pageOf([row(1)]));
    serve(1, conversationOf(1, { posts: [post(4)] }));
    const sent = posts(stored(5));
    start("/messages/1");
    await shown(1);
    button("Send", detail()).click();
    await until(() => {
      expect(subject().getAttribute("aria-invalid")).toBe("true");
    });
    expect(errorOf(subject())).toBe("Enter a subject.");
    expect(document.activeElement).toBe(subject());

    await write("x".repeat(121), "é".repeat(33_000));
    button("Send", detail()).click();
    await until(() => {
      expect(errorOf(message())).toBe("Shorten the message to 64\u00a0KiB. It is 64.5\u00a0KiB.");
    });
    expect(errorOf(subject())).toBe("Shorten the subject to 120 characters. It has 121.");
    const summary = detail().querySelector('form .cp-notice[data-tone="danger"]') as HTMLElement;
    expect(summary.hidden).toBe(false);
    expect(summary.textContent).toContain("2 fields need a fix before this sends");
    expect(document.activeElement).toBe(summary);

    await userEvent.fill(subject(), "Fine");
    expect(subject().hasAttribute("aria-invalid")).toBe(false);
    expect(sent.bodies).toEqual([]);
    expect(entries().length).toBe(1);
  });

  it("counts the message's bytes only near the 64 KiB limit", async () => {
    answer(pageOf([row(1)]));
    serve(1, conversationOf(1, { posts: [post(4)] }));
    start("/messages/1");
    await shown(1);
    const count = detail().querySelector("#compose-body-count") as HTMLElement;
    await userEvent.fill(message(), "a".repeat(58_000));
    expect(count.textContent).toBe("");
    await userEvent.fill(message(), "a".repeat(60_000));
    expect(count.textContent).toBe("58.6\u00a0KiB of 64\u00a0KiB");
  });

  it("sends on Ctrl+Enter from the message, and Enter alone breaks the line", async () => {
    answer(pageOf([row(1)]));
    serve(1, conversationOf(1, { posts: [post(4)] }));
    const sent = posts(stored(5));
    start("/messages/1");
    await shown(1);
    await userEvent.fill(subject(), "Merge order");
    await userEvent.click(message());
    await userEvent.keyboard("Now{Enter}then");
    expect(message().value).toBe("Now\nthen");
    expect(sent.bodies).toEqual([]);
    await userEvent.keyboard("{Control>}{Enter}{/Control}");
    await until(() => {
      expect(sent.bodies).toEqual([
        { to: { path: "/workspace/r1" }, subject: "Merge order", body: "Now\nthen" },
      ]);
    });
  });

  it("keeps an unsent draft for its conversation when another one opens", async () => {
    answer(pageOf([row(1), row(2)]));
    serve(1, conversationOf(1, { posts: [post(4)] }));
    serve(2, conversationOf(2, { posts: [post(6, { thread: 2 })] }));
    const router = start("/messages/1");
    await shown(1);
    await write("Half", "written");

    router.go({ screen: "messages", show: "all", conversation: 2 });
    await until(() => {
      expect(detail().querySelector("h2")?.textContent).toBe("/workspace/r2");
    });
    await shown(1);
    expect(subject().value).toBe("");

    router.go({ screen: "messages", show: "all", conversation: 1 });
    await until(() => {
      expect(subject().value).toBe("Half");
    });
    expect(message().value).toBe("written");
  });

  describe("a send on its way when another conversation opens and this one opens again", () => {
    async function away(router: ReturnType<typeof start>): Promise<void> {
      router.go({ screen: "messages", show: "all", conversation: 2 });
      await until(() => {
        expect(detail().querySelector("h2")?.textContent).toBe("/workspace/r2");
      });
      router.go({ screen: "messages", show: "all", conversation: 1 });
      await until(() => {
        expect(detail().querySelector("h2")?.textContent).toBe("/workspace/r1");
      });
      await shown(2);
    }

    async function sentThenAway(then: Answer): Promise<ReturnType<typeof heldPosts>> {
      answer(pageOf([row(1), row(2)]));
      serve(2, conversationOf(2, { posts: [post(6, { thread: 2 })] }));
      const held = heldConversation(then);
      const router = start("/messages/1");
      await shown(1);
      await write("Merge order", "Now.");
      button("Send", detail()).click();
      await until(() => {
        expect(held.seen.keys.length).toBe(1);
      });
      await away(router);
      return held;
    }

    it("still shows it sending, sends it once, and clears the fields once it is stored", async () => {
      const { seen, release } = await sentThenAway(stored(5));
      const own = entries()[1] as HTMLElement;
      expect(own.getAttribute("data-state")).toBe("sending");
      expect(own.querySelector("h3")?.textContent).toBe("Merge order");
      await until(() => {
        expect(button("Sending…", detail()).getAttribute("aria-busy")).toBe("true");
      });
      message().focus();
      await userEvent.keyboard("{Control>}{Enter}{/Control}");
      expect(seen.keys.length).toBe(1);

      release();
      await until(() => {
        expect(entries()[1]?.querySelector("footer")?.textContent).toBe("Read by 1 of 2");
      });
      expect(entries().length).toBe(2);
      expect(subject().value).toBe("");
      expect(message().value).toBe("");
      expect(sessionStorage.getItem("grelot.draft.1")).toBeNull();
      expect(seen.keys.length).toBe(1);
    });

    it("shows it Not sent when it fails meanwhile, and Retry sends it under its first key", async () => {
      const { seen, release } = await sentThenAway(storeFull);
      release();
      await until(() => {
        expect(entries()[1]?.getAttribute("data-state")).toBe("failed");
      });
      expect(errorOf(message())).toBe("The store is full: new writes stop at 90 % of 512 MiB.");
      expect(subject().value).toBe("Merge order");

      button("Retry", entries()[1] as HTMLElement).click();
      await until(() => {
        expect(seen.keys.length).toBe(2);
      });
      expect(seen.keys[1]).toBe(seen.keys[0]);
      expect(seen.bodies[1]).toEqual(seen.bodies[0]);
      await until(() => {
        expect(message().value).toBe("");
      });
    });

    it("takes Send on the same text after it failed as that post, under its first key", async () => {
      answer(pageOf([row(1), row(2)]));
      serve(1, conversationOf(1, { posts: [post(4)] }));
      serve(2, conversationOf(2, { posts: [post(6, { thread: 2 })] }));
      const held = heldPosts(storeFull);
      const router = start("/messages/1");
      await shown(1);
      await write("Merge order", "Now.");
      button("Send", detail()).click();
      await until(() => {
        expect(held.seen.keys.length).toBe(1);
      });
      router.go({ screen: "messages", show: "all", conversation: 2 });
      await until(() => {
        expect(detail().querySelector("h2")?.textContent).toBe("/workspace/r2");
      });
      held.release();
      router.go({ screen: "messages", show: "all", conversation: 1 });
      await until(() => {
        expect(entries()[1]?.getAttribute("data-state")).toBe("failed");
      });
      expect(errorOf(message())).toBe("The store is full: new writes stop at 90 % of 512 MiB.");

      (await rested("Send", detail())).click();
      await until(() => {
        expect(held.seen.keys.length).toBe(2);
      });
      expect(held.seen.keys[1]).toBe(held.seen.keys[0]);
    });

    it("no longer draws it when the conversation, read after it was stored, no longer holds it", async () => {
      answer(pageOf([row(1), row(2)]));
      let expired = false;
      worker.use(
        http.get("/api/conversations/1", () =>
          HttpResponse.json(conversationOf(1, { posts: expired ? [] : [post(4)] })),
        ),
      );
      serve(2, conversationOf(2, { posts: [post(6, { thread: 2 })] }));
      const held = heldPosts(stored(5));
      const router = start("/messages/1");
      await shown(1);
      await write("Merge order", "Now.");
      button("Send", detail()).click();
      await until(() => {
        expect(held.seen.keys.length).toBe(1);
      });
      router.go({ screen: "messages", show: "all", conversation: 2 });
      await until(() => {
        expect(detail().querySelector("h2")?.textContent).toBe("/workspace/r2");
      });
      held.release();
      await until(() => {
        expect(sessionStorage.getItem("grelot.draft.1")).toBeNull();
      });
      expired = true;

      router.go({ screen: "messages", show: "all", conversation: 1 });
      await until(() => {
        expect(detail().textContent).toContain("No messages in the last 72 hours.");
      });
      expect(entries()).toEqual([]);
    });
  });

  describe("a send on its way when another screen opens and Messages opens again", () => {
    /** Follows the first post's sender link to the agent's page and comes back to conversation 1. */
    async function roundTrip(
      router: ReturnType<typeof start>,
      there: () => void = () => undefined,
    ): Promise<void> {
      worker.use(
        http.get("/api/agents/bravo", () =>
          HttpResponse.json({
            agent: {
              name: "bravo",
              description: "",
              state: "live",
              paths: [],
              follows: [],
              registered_ms: at,
            },
            reservations: [],
            delegates: [],
          }),
        ),
        http.get("/api/agents/bravo/history", () => HttpResponse.json({ posts: [] })),
      );
      (entries()[0]?.querySelector("header a") as HTMLAnchorElement).click();
      await until(() => {
        expect(location.pathname).toBe("/agents/bravo");
        expect(h.root.querySelector(".cp-detail")).toBeNull();
      });
      there();
      router.go({ screen: "messages", show: "all", conversation: 1 });
      await until(() => {
        expect(detail().querySelector("h2")?.textContent).toBe("/workspace/r1");
      });
    }

    async function sending(then: Answer): Promise<{
      router: ReturnType<typeof start>;
      held: ReturnType<typeof heldPosts>;
    }> {
      answer(pageOf([row(1)]));
      const held = heldConversation(then);
      const router = start("/messages/1");
      await shown(1);
      await write("Merge order", "Now.");
      button("Send", detail()).click();
      await until(() => {
        expect(held.seen.keys.length).toBe(1);
      });
      return { router, held };
    }

    it("still shows it sending, sends it once, and clears the fields once it is stored", async () => {
      const { router, held } = await sending(stored(5));
      await roundTrip(router);
      await shown(2);
      expect(entries()[1]?.getAttribute("data-state")).toBe("sending");
      await until(() => {
        expect(button("Sending…", detail()).getAttribute("aria-busy")).toBe("true");
      });
      message().focus();
      await userEvent.keyboard("{Control>}{Enter}{/Control}");
      expect(held.seen.keys.length).toBe(1);

      held.release();
      await until(() => {
        expect(entries()[1]?.querySelector("footer")?.textContent).toBe("Read by 1 of 2");
      });
      expect(entries().length).toBe(2);
      expect(subject().value).toBe("");
      expect(message().value).toBe("");
      expect(sessionStorage.getItem("grelot.draft.1")).toBeNull();
      expect(held.seen.keys.length).toBe(1);
    });

    it("shows it Not sent with the server's words when it failed there, and Retry sends it under its first key", async () => {
      const { router, held } = await sending(storeFull);
      await roundTrip(router, held.release);
      await until(() => {
        expect(entries()[1]?.getAttribute("data-state")).toBe("failed");
      });
      expect(errorOf(message())).toBe("The store is full: new writes stop at 90 % of 512 MiB.");
      expect(subject().value).toBe("Merge order");

      button("Retry", entries()[1] as HTMLElement).click();
      await until(() => {
        expect(held.seen.keys.length).toBe(2);
      });
      expect(held.seen.keys[1]).toBe(held.seen.keys[0]);
      expect(held.seen.bodies[1]).toEqual(held.seen.bodies[0]);
      await until(() => {
        expect(message().value).toBe("");
      });
    });
  });

  it("offers each side of two agents' conversation a new message in place of the composer", async () => {
    answer(
      pageOf([row(3, { kind: "direct", title: "@alpha and @bravo", agents: ["alpha", "bravo"] })]),
    );
    serve(3, conversationOf(3, { posts: [post(4, { thread: 3 })] }));
    start("/messages/3");
    await shown(1);
    expect(subject()).toBeNull();
    const foot = detail().querySelector(".cp-feed-foot") as HTMLElement;
    expect(foot.querySelector("p")?.textContent).toBe("Only @alpha and @bravo write here.");

    button("Message @bravo", foot).click();
    const dialog = document.getElementById("new-message") as HTMLDialogElement;
    await until(() => {
      expect(dialog.open).toBe(true);
    });
    expect((document.getElementById("new-message-to") as HTMLInputElement).value).toBe("@bravo");
    expect(document.activeElement).toBe(document.getElementById("new-message-subject"));
  });
});
