import { delay, http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { until } from "../__test-helpers__/until.js";
import {
  answer,
  at,
  button,
  emit,
  h,
  hour,
  links,
  pageOf,
  restart,
  rested,
  row,
  rowsShown,
  start,
  titles,
  useMessagesHarness,
  worker,
} from "./__test-helpers__/harness.js";

useMessagesHarness();

const list = (): HTMLElement =>
  h.root.querySelector('ul.cp-list[aria-label="Conversations"]') as HTMLElement;
const railFilter = (word: string): HTMLButtonElement =>
  [...h.root.querySelectorAll<HTMLButtonElement>("aside.cp-rail button.cp-list-row")].find((b) =>
    b.textContent.startsWith(word),
  ) as HTMLButtonElement;
const emptyHeading = (): string | null => h.root.querySelector(".cp-empty h2")?.textContent ?? null;

describe("the Messages list", () => {
  it("draws a row per conversation: its title, last sender and text, participants, unread and time", async () => {
    answer(
      pageOf([
        row(3, {
          participants: 3,
          unread: 2,
          last: { id: 30, from: "bravo", subject: "q", preview: "Queue moves", at_ms: at },
        }),
        row(2, {
          kind: "direct",
          title: "@alpha and @user",
          agents: ["alpha", "user"],
          last: { id: 20, from: "user", subject: "Hello", preview: "", at_ms: at - hour },
        }),
        row(1, {
          kind: "announce",
          title: "announce",
          last: { id: 10, from: "alpha", subject: "d", preview: "Deploy at 3", at_ms: at },
        }),
      ]),
    );
    start("/messages");
    await rowsShown(["/workspace/r3", "You and @alpha", "Announcements"]);

    const [path, direct, announce] = links();
    expect(path?.querySelector(".cp-truncate")?.classList.contains("cp-mono")).toBe(true);
    expect(direct?.querySelector(".cp-truncate")?.classList.contains("cp-mono")).toBe(false);
    const pathTitle = path?.querySelector(".cp-truncate");
    expect(pathTitle?.getAttribute("data-cut")).toBe("start");
    expect(pathTitle?.getAttribute("dir")).toBe("ltr");
    expect(pathTitle?.getAttribute("data-cp-tip")).toBe("/workspace/r3");
    const directTitle = direct?.querySelector(".cp-truncate");
    expect(directTitle?.hasAttribute("data-cut")).toBe(false);
    expect(directTitle?.hasAttribute("data-cp-tip")).toBe(false);
    expect(directTitle?.getAttribute("dir")).toBe("auto");
    expect(links().map((a) => a.querySelector("small")?.textContent)).toEqual([
      "@bravo: Queue moves",
      "You: Hello",
      "@alpha: Deploy at 3",
    ]);
    expect(path?.hasAttribute("data-unread")).toBe(true);
    expect(path?.querySelector(".cp-badge")?.textContent).toBe("2 unread");
    expect(direct?.hasAttribute("data-unread")).toBe(false);
    expect(direct?.querySelector(".cp-badge")).toBeNull();
    expect(path?.querySelector(".cp-meta")?.textContent).toBe("3 participants");
    expect(direct?.querySelector("time")?.getAttribute("datetime")).toBe(
      "2026-11-13T08:40:00.000Z",
    );
    expect(announce?.getAttribute("href")).toBe("/messages/1");
    expect(list().hasAttribute("aria-busy")).toBe(false);
  });

  it("a filter press replaces show in the address, lists that filter's conversations and keeps the focus", async () => {
    const both = { threads: 2, direct: 1 };
    const direct = row(5, { kind: "direct", title: "@a and @b", agents: ["a", "b"] });
    answer(pageOf([direct, row(4), row(3)], { counts: both }), pageOf([direct], { counts: both }));
    start("/messages");
    await rowsShown(["@a and @b", "/workspace/r4", "/workspace/r3"]);
    expect(railFilter("All").textContent).toBe("All3 conversations");
    expect(railFilter("Threads").textContent).toBe("Threads2 conversations");
    expect(railFilter("Direct").textContent).toBe("Direct1 conversation");

    const entries = history.length;
    railFilter("Direct").focus();
    railFilter("Direct").click();
    await rowsShown(["@a and @b"]);
    expect(location.pathname + location.search).toBe("/messages?show=direct");
    expect(history.length).toBe(entries);
    expect(h.asked.at(-1)?.get("kind")).toBe("direct");
    expect(railFilter("Direct").getAttribute("aria-pressed")).toBe("true");
    expect(railFilter("All").getAttribute("aria-pressed")).toBe("false");
    expect(document.activeElement).toBe(railFilter("Direct"));
    expect(links()[0]?.getAttribute("href")).toBe("/messages/5?show=direct");
  });

  it("marks the open conversation's row, and only it, and titles the detail from it", async () => {
    answer(
      pageOf([
        row(3),
        row(2, { kind: "direct", title: "@alpha and @user", agents: ["alpha", "user"] }),
      ]),
    );
    const router = start("/messages/2");
    await rowsShown(["/workspace/r3", "You and @alpha"]);
    await until(() => {
      expect(links().map((a) => a.getAttribute("aria-current"))).toEqual([null, "true"]);
    });
    const detail = h.root.querySelector(".cp-detail") as HTMLElement;
    expect(detail.querySelector("h2")?.textContent).toBe("You and @alpha");
    expect(detail.querySelector(".cp-chip")?.textContent).toBe("Direct");

    router.go({ screen: "messages", show: "all", conversation: 3 });
    await until(() => {
      expect(links().map((a) => a.getAttribute("aria-current"))).toEqual(["true", null]);
    });
    expect(detail.querySelector("h2")?.textContent).toBe("/workspace/r3");
    expect(detail.querySelector(".cp-chip")?.textContent).toBe("Thread");
  });

  it("says a conversation the list does not hold is gone when the server no longer has it", async () => {
    answer(pageOf([row(3)]));
    worker.use(
      http.get("/api/conversations/9", () =>
        HttpResponse.json({ error: "no conversation 9", code: "not_found" }, { status: 404 }),
      ),
    );
    start("/messages/9");
    await until(() => {
      expect(h.root.querySelector(".cp-detail h2")?.textContent).toBe("This conversation is gone.");
    });
  });

  it("names the filter and the retention when nothing is listed, and offers to clear a filter", async () => {
    answer(pageOf([], { counts: { threads: 0, direct: 0 } }));
    start("/messages");
    await until(() => {
      expect(emptyHeading()).toBe("No conversations in the last 72 hours.");
    });
    expect(h.root.querySelector(".cp-empty button")?.textContent).toBe("New message");
    restart();

    answer(pageOf([], { counts: { threads: 2, direct: 0 } }));
    start("/messages?show=direct");
    await until(() => {
      expect(emptyHeading()).toBe("No direct conversations in the last 72 hours.");
    });
    button("Clear filters", h.root.querySelector(".cp-empty") as HTMLElement).click();
    await until(() => {
      expect(location.pathname + location.search).toBe("/messages");
    });
    await until(() => {
      expect(h.asked.at(-1)?.get("kind")).toBe("all");
    });
    await until(() => {
      expect(emptyHeading()).toBe("No conversations in the last 72 hours.");
    });
  });

  it("points a server no agent has reached and nobody has posted to at the machine setup guide", async () => {
    worker.use(http.get("/api/agents", () => HttpResponse.json({ agents: [] })));
    start("/messages");
    await until(() => {
      expect(emptyHeading()).toBe("No agent has registered yet.");
    });
    const guide = h.root.querySelector<HTMLAnchorElement>(".cp-empty a");
    expect(guide?.textContent).toBe("Set up a machine");
    expect(guide?.href).toBe("https://github.com/cplieger/grelot/blob/main/docs/client.md");
    restart();

    answer(pageOf([], { counts: { threads: 2, direct: 0 } }));
    start("/messages?show=direct");
    await until(() => {
      expect(emptyHeading()).toBe("No direct conversations in the last 72 hours.");
    });
  });

  it("shows a failed load in a notice with the server's words, and Retry loads again", async () => {
    h.answers = [
      () =>
        HttpResponse.json(
          { error: "The store is not open yet.", code: "timeout" },
          { status: 503 },
        ),
      () => HttpResponse.json(pageOf([row(1)])),
    ];
    start("/messages");
    await until(() => {
      expect(h.root.querySelector('.cp-notice[data-tone="danger"]')?.textContent).toContain(
        "Conversations could not be loaded.",
      );
    });
    const notice = h.root.querySelector('.cp-notice[data-tone="danger"]') as HTMLElement;
    expect(notice.textContent).toContain("The store is not open yet.");
    button("Retry", notice).click();
    await rowsShown(["/workspace/r1"]);
    expect(h.root.querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
  });

  it("a Retry pressed twice on a failed load sends one request", async () => {
    const held = Promise.withResolvers<undefined>();
    h.answers = [
      () => HttpResponse.json({ error: "The store is busy.", code: "timeout" }, { status: 503 }),
      async () => {
        await held.promise;
        return HttpResponse.json(pageOf([row(1)]));
      },
    ];
    start("/messages");
    const retry = await until(() =>
      button("Retry", h.root.querySelector('.cp-notice[data-tone="danger"]') as HTMLElement),
    );
    retry.click();
    retry.click();
    await until(() => {
      expect(h.asked.length).toBe(2);
    });
    held.resolve(undefined);
    await rowsShown(["/workspace/r1"]);
    expect(h.asked.length).toBe(2);
  });

  it("draws skeleton rows while a slow first page loads", async () => {
    h.answers = [
      async () => {
        await delay(900);
        return HttpResponse.json(pageOf([row(1)]));
      },
    ];
    start("/messages");
    await until(() => {
      expect(h.root.querySelectorAll(".cp-list-row[data-skeleton]").length).toBe(6);
    });
    expect(h.root.querySelector(".cp-list[aria-busy='true']")).not.toBeNull();
    await rowsShown(["/workspace/r1"]);
    expect(h.root.querySelector(".cp-list-row[data-skeleton]")).toBeNull();
  });

  it("Show older asks for the page after the last row and adds it, then leaves at the oldest", async () => {
    answer(pageOf([row(3), row(2)], { next: 20, next_thread: 2 }), pageOf([row(1)]));
    start("/messages");
    await rowsShown(["/workspace/r3", "/workspace/r2"]);
    const older = await rested("Show older");
    expect(older.closest("li")?.classList.contains("cp-list-foot")).toBe(true);
    expect(older.closest("li")).toBe(list().lastElementChild);

    older.focus();
    older.click();
    await rowsShown(["/workspace/r3", "/workspace/r2", "/workspace/r1"]);
    expect(h.asked.at(-1)?.get("before")).toBe("20");
    expect(h.asked.at(-1)?.get("before_thread")).toBe("2");
    await until(() => {
      expect(h.root.querySelector(".cp-list-foot")).toBeNull();
    });
    expect(document.activeElement).toBe(links()[2]);
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "1 older conversation",
      );
    });
  });

  it("drops a Show older page that lands after a filter change, and a filter with one page has no foot", async () => {
    const late = Promise.withResolvers<undefined>();
    h.answers = [
      () => HttpResponse.json(pageOf([row(3), row(2)], { next: 20, next_thread: 2 })),
      async () => {
        await late.promise;
        return HttpResponse.json(pageOf([row(1)]));
      },
      () => HttpResponse.json(pageOf([row(4)], { counts: { threads: 1, direct: 0 } })),
    ];
    start("/messages");
    await rowsShown(["/workspace/r3", "/workspace/r2"]);
    (await rested("Show older")).click();
    await until(() => {
      expect(h.asked).toHaveLength(2);
    });
    railFilter("Threads").click();
    await rowsShown(["/workspace/r4"]);
    expect(h.root.querySelector(".cp-list-foot")).toBeNull();
    late.resolve(undefined);
    await delay(50);
    expect(titles()).toEqual(["/workspace/r4"]);
  });

  it("a change applies at once while the reader is elsewhere", async () => {
    answer(
      pageOf([row(3), row(2)]),
      pageOf([row(2, { last: { ...row(2).last, id: 99 } }), row(3)]),
    );
    start("/messages");
    await rowsShown(["/workspace/r3", "/workspace/r2"]);
    const kept = links()[0];
    emit({ conversations: [2], agents: false });
    await rowsShown(["/workspace/r2", "/workspace/r3"]);
    expect(links()[1]).toBe(kept);
    expect(h.root.querySelector(".cp-list-pending")).toBeNull();
  });

  it("holds a change back while the reader is in the list, until Show puts it in", async () => {
    answer(
      pageOf([row(3), row(2)]),
      pageOf([row(2, { last: { ...row(2).last, id: 99 } }), row(3)]),
    );
    start("/messages");
    await rowsShown(["/workspace/r3", "/workspace/r2"]);
    links()[0]?.focus();
    emit({ conversations: [2], agents: false });
    await until(() => {
      expect(h.root.querySelector(".cp-list-pending")?.textContent).toBe(
        "New messages in 1 conversation.Show",
      );
    });
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "New messages in 1 conversation.",
      );
    });
    expect(titles()).toEqual(["/workspace/r3", "/workspace/r2"]);

    button("Show", h.root.querySelector(".cp-list-pending") as HTMLElement).click();
    await rowsShown(["/workspace/r2", "/workspace/r3"]);
    expect(h.root.querySelector(".cp-list-pending")).toBeNull();
    expect(document.activeElement).toBe(links()[0]);
  });

  it("a second change while one waits counts the conversations, and Show draws the newest", async () => {
    answer(
      pageOf([row(3), row(2)]),
      pageOf([row(2, { last: { ...row(2).last, id: 98, preview: "newer" } }), row(3)]),
      pageOf([
        row(3, { last: { ...row(3).last, id: 99, preview: "newest" } }),
        row(2, { last: { ...row(2).last, id: 98, preview: "newer" } }),
      ]),
    );
    start("/messages");
    await rowsShown(["/workspace/r3", "/workspace/r2"]);
    links()[0]?.focus();
    emit({ conversations: [2], agents: false });
    await until(() => {
      expect(h.root.querySelector(".cp-list-pending")?.textContent).toBe(
        "New messages in 1 conversation.Show",
      );
    });
    emit({ conversations: [3], agents: false });
    await until(() => {
      expect(h.root.querySelector(".cp-list-pending")?.textContent).toBe(
        "New messages in 2 conversations.Show",
      );
    });
    expect(titles()).toEqual(["/workspace/r3", "/workspace/r2"]);
    button("Show", h.root.querySelector(".cp-list-pending") as HTMLElement).click();
    await rowsShown(["/workspace/r3", "/workspace/r2"]);
    expect(links().map((a) => a.querySelector("small")?.textContent)).toEqual([
      "@bravo: newest",
      "@bravo: newer",
    ]);
    expect(h.root.querySelector(".cp-list-pending")).toBeNull();
  });

  it("a Show older pressed while a change waits puts its page in what Show draws", async () => {
    const moved = row(2, { last: { ...row(2).last, id: 99 } });
    answer(
      pageOf([row(3), row(2)], { next: 20, next_thread: 2 }),
      pageOf([moved, row(3), row(1)], { next: 10, next_thread: 1 }),
      pageOf([row(1)]),
      pageOf([moved, row(3), row(1)], { counts: { threads: 4, direct: 0 } }),
    );
    start("/messages");
    await rowsShown(["/workspace/r3", "/workspace/r2"]);
    links()[0]?.focus();
    emit({ conversations: [2], agents: false });
    await until(() => {
      expect(h.root.querySelector(".cp-list-pending")?.textContent).toBe(
        "New messages in 1 conversation.Show",
      );
    });

    const older = await rested("Show older");
    older.focus();
    older.click();
    await rowsShown(["/workspace/r3", "/workspace/r2", "/workspace/r1"]);
    await until(() => {
      expect(railFilter("All").textContent).toBe("All4 conversations");
    });
    button("Show", h.root.querySelector(".cp-list-pending") as HTMLElement).click();
    await rowsShown(["/workspace/r2", "/workspace/r3", "/workspace/r1"]);
    expect(h.root.querySelector(".cp-list-foot")).toBeNull();
  });

  it("a Show pressed before the follow-up read of a Show older lands keeps the older page", async () => {
    const moved = row(2, { last: { ...row(2).last, id: 99 } });
    const followUp = Promise.withResolvers<undefined>();
    h.answers = [
      () => HttpResponse.json(pageOf([row(3), row(2)], { next: 20, next_thread: 2 })),
      () => HttpResponse.json(pageOf([moved, row(3)], { next: 30, next_thread: 3 })),
      () => HttpResponse.json(pageOf([row(1)], { next: 10, next_thread: 1 })),
      () => HttpResponse.json(pageOf([row(1)])),
      async () => {
        await followUp.promise;
        return HttpResponse.json(pageOf([moved, row(3), row(1)]));
      },
    ];
    start("/messages");
    await rowsShown(["/workspace/r3", "/workspace/r2"]);
    links()[0]?.focus();
    emit({ conversations: [2], agents: false });
    await until(() => {
      expect(h.root.querySelector(".cp-list-pending")?.textContent).toBe(
        "New messages in 1 conversation.Show",
      );
    });

    (await rested("Show older")).click();
    await rowsShown(["/workspace/r3", "/workspace/r2", "/workspace/r1"]);
    await until(() => {
      expect(h.asked).toHaveLength(5);
    });
    button("Show", h.root.querySelector(".cp-list-pending") as HTMLElement).click();

    await rowsShown(["/workspace/r2", "/workspace/r3", "/workspace/r1"]);
    expect(h.root.querySelector(".cp-list-foot")).toBeNull();
    followUp.resolve(undefined);
    await delay(50);
    expect(titles()).toEqual(["/workspace/r2", "/workspace/r3", "/workspace/r1"]);
  });

  it("a refresh folds the newest page into the older pages already shown", async () => {
    const moved = row(3, { last: { ...row(3).last, id: 99 } });
    answer(
      pageOf([row(5), row(4)], { next: 40, next_thread: 4 }),
      pageOf([row(3), row(2)], { next: 20, next_thread: 2 }),
      pageOf([moved, row(5)], { next: 50, next_thread: 5 }),
      pageOf([row(4), row(2)], { next: 20, next_thread: 2 }),
      pageOf([row(1)]),
    );
    start("/messages");
    await rowsShown(["/workspace/r5", "/workspace/r4"]);
    (await rested("Show older")).click();
    await rowsShown(["/workspace/r5", "/workspace/r4", "/workspace/r3", "/workspace/r2"]);
    // Show older focuses the first row it added, which puts the reader in the list.
    await until(() => {
      expect(document.activeElement).toBe(links()[2]);
    });
    links()[2]?.blur();

    emit({ conversations: [3], agents: false });
    await rowsShown(["/workspace/r3", "/workspace/r5", "/workspace/r4", "/workspace/r2"]);
    (await rested("Show older")).click();
    await rowsShown([
      "/workspace/r3",
      "/workspace/r5",
      "/workspace/r4",
      "/workspace/r2",
      "/workspace/r1",
    ]);
    expect(h.asked.at(-1)?.get("before")).toBe("20");
  });

  it("a refresh reads the older pages again: an expired row leaves, a changed one is redrawn in place", async () => {
    answer(
      pageOf([row(5), row(4)], { next: 40, next_thread: 4 }),
      pageOf([row(3), row(2)], { next: 20, next_thread: 2 }),
      pageOf([row(5), row(4)], { next: 40, next_thread: 4 }),
      pageOf([row(3, { unread: 2 }), row(1)], { next: 10, next_thread: 1 }),
      pageOf([row(1)]),
    );
    start("/messages");
    await rowsShown(["/workspace/r5", "/workspace/r4"]);
    (await rested("Show older")).click();
    await rowsShown(["/workspace/r5", "/workspace/r4", "/workspace/r3", "/workspace/r2"]);
    const third = links()[2];
    expect(third?.querySelector(".cp-badge")).toBeNull();

    emit({ conversations: [5], agents: false });
    await rowsShown(["/workspace/r5", "/workspace/r4", "/workspace/r3"]);
    expect(links()[2]).toBe(third);
    expect(third?.querySelector(".cp-badge")?.textContent).toBe("2 unread");
    expect(h.asked.at(-1)?.get("before")).toBe("40");
    expect(h.asked.at(-1)?.get("before_thread")).toBe("4");

    (await rested("Show older")).click();
    await rowsShown(["/workspace/r5", "/workspace/r4", "/workspace/r3", "/workspace/r1"]);
    expect(h.asked.at(-1)?.get("before")).toBe("30");
    expect(h.asked.at(-1)?.get("before_thread")).toBe("3");
  });

  it("keeps the rows when a refresh fails, says so, and Retry refreshes", async () => {
    h.answers = [
      () => HttpResponse.json(pageOf([row(1)])),
      () => HttpResponse.json({ error: "The store is busy.", code: "timeout" }, { status: 503 }),
      () => HttpResponse.json(pageOf([row(2), row(1)])),
    ];
    start("/messages");
    await rowsShown(["/workspace/r1"]);
    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(h.root.querySelector('.cp-notice[data-tone="danger"]')?.textContent).toBe(
        "The list could not be refreshed. The store is busy.Retry",
      );
    });
    expect(titles()).toEqual(["/workspace/r1"]);
    button("Retry", h.root.querySelector('.cp-notice[data-tone="danger"]') as HTMLElement).click();
    await rowsShown(["/workspace/r2", "/workspace/r1"]);
    expect(h.root.querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
  });

  it("keeps the first-run empty state when a refresh fails, says so", async () => {
    worker.use(http.get("/api/agents", () => HttpResponse.json({ agents: [] })));
    h.answers = [
      () => HttpResponse.json(pageOf([], { counts: { threads: 0, direct: 0 } })),
      () => HttpResponse.json({ error: "The store is busy.", code: "timeout" }, { status: 503 }),
    ];
    start("/messages");
    await until(() => {
      expect(emptyHeading()).toBe("No agent has registered yet.");
    });
    emit({ conversations: [], agents: true });
    await until(() => {
      expect(h.root.querySelector('.cp-notice[data-tone="danger"]')?.textContent).toBe(
        "The list could not be refreshed. The store is busy.Retry",
      );
    });
    expect(emptyHeading()).toBe("No agent has registered yet.");
    expect(h.root.querySelector(".cp-empty a")?.textContent).toBe("Set up a machine");
  });

  it("keeps the rows, the counts and a held-back change when a refresh's older page fails", async () => {
    const busy = (): Response =>
      HttpResponse.json({ error: "The store is busy.", code: "timeout" }, { status: 503 });
    h.answers = [
      () => HttpResponse.json(pageOf([row(2), row(1)])),
      () =>
        HttpResponse.json(
          pageOf([row(3), row(2)], { next: 20, next_thread: 2, counts: { threads: 3, direct: 0 } }),
        ),
      () => HttpResponse.json(pageOf([row(1)])),
      () =>
        HttpResponse.json(
          pageOf([row(4), row(3)], { next: 30, next_thread: 3, counts: { threads: 4, direct: 0 } }),
        ),
      busy,
    ];
    start("/messages");
    await rowsShown(["/workspace/r2", "/workspace/r1"]);
    links()[0]?.focus();
    emit({ conversations: [3], agents: false });
    await until(() => {
      expect(h.root.querySelector(".cp-list-pending")?.textContent).toBe(
        "New messages in 1 conversation.Show",
      );
    });
    expect(railFilter("All").textContent).toBe("All3 conversations");

    emit({ conversations: [4], agents: false });
    await until(() => {
      expect(h.root.querySelector('.cp-notice[data-tone="danger"]')?.textContent).toBe(
        "The list could not be refreshed. The store is busy.Retry",
      );
    });
    expect(h.asked.at(-1)?.get("before")).toBe("30");
    expect(titles()).toEqual(["/workspace/r2", "/workspace/r1"]);
    expect(railFilter("All").textContent).toBe("All3 conversations");
    expect(h.root.querySelector(".cp-list-pending")?.textContent).toBe(
      "New messages in 1 conversation.Show",
    );
  });

  it("says a failed Show older, keeps what it showed, and its button reads Retry", async () => {
    h.answers = [
      () => HttpResponse.json(pageOf([row(2)], { next: 20, next_thread: 2 })),
      () => HttpResponse.json({ error: "The store is busy.", code: "timeout" }, { status: 503 }),
    ];
    start("/messages");
    await rowsShown(["/workspace/r2"]);
    (await rested("Show older")).click();
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "Older conversations could not be loaded.",
      );
    });
    expect(titles()).toEqual(["/workspace/r2"]);
    expect((await rested("Retry")).closest(".cp-list-foot")).toBe(list().lastElementChild);
  });

  it("says how many conversations a filter lists once they load", async () => {
    answer(pageOf([row(1)], { counts: { threads: 1, direct: 0 } }));
    start("/messages");
    await rowsShown(["/workspace/r1"]);
    railFilter("Threads").click();
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "1 conversation.",
      );
    });
  });

  it("a filter press with a conversation open leaves the focus on the filter", async () => {
    answer(pageOf([row(1)]));
    start("/messages/1");
    await rowsShown(["/workspace/r1"]);
    railFilter("Threads").focus();
    railFilter("Threads").click();
    await until(() => {
      expect(location.pathname + location.search).toBe("/messages/1?show=threads");
    });
    await until(() => {
      expect(railFilter("Threads").getAttribute("aria-pressed")).toBe("true");
    });
    expect(document.activeElement).toBe(railFilter("Threads"));
    // The next test's answers would go to this reload.
    await until(() => {
      expect(list().hasAttribute("aria-busy")).toBe(false);
    });
  });

  it("fetches a change that lands while the first page loads, once that page is shown", async () => {
    const held = Promise.withResolvers<undefined>();
    h.answers = [
      async () => {
        await held.promise;
        return HttpResponse.json(pageOf([row(1)]));
      },
      () => HttpResponse.json(pageOf([row(2), row(1)])),
    ];
    start("/messages");
    await until(() => {
      expect(h.asked.length).toBe(1);
    });
    emit({ conversations: [2], agents: false });
    held.resolve(undefined);
    await rowsShown(["/workspace/r2", "/workspace/r1"]);
  });

  it("a refresh rewrites a changed row in place and keeps an unchanged row's time", async () => {
    const moved = row(2, { last: { ...row(2).last, preview: "changed" } });
    answer(pageOf([row(1), row(2)]), pageOf([row(3), row(1), moved]));
    start("/messages");
    await rowsShown(["/workspace/r1", "/workspace/r2"]);
    const time = links()[0]?.querySelector("time");
    const preview = links()[1]?.querySelector("small");
    expect(time).not.toBeNull();

    emit({ conversations: [3], agents: false });
    await rowsShown(["/workspace/r3", "/workspace/r1", "/workspace/r2"]);
    expect(links()[1]?.querySelector("time")).toBe(time);
    expect(links()[2]?.querySelector("small")).toBe(preview);
    expect(preview?.textContent).toBe("@bravo: changed");
  });

  it("a refresh moves a row's time to its newer last post", async () => {
    const later = at + hour;
    answer(pageOf([row(1)]), pageOf([row(1, { last: { ...row(1).last, id: 11, at_ms: later } })]));
    start("/messages");
    await rowsShown(["/workspace/r1"]);
    const time = links()[0]?.querySelector("time");
    expect(time?.dateTime).toBe(new Date(at).toISOString());

    emit({ conversations: [1], agents: false });
    await until(() => {
      expect(time?.dateTime).toBe(new Date(later).toISOString());
    });
    expect(links()[0]?.querySelector("time")).toBe(time);
  });

  it("a resync during the first load replaces it, and settles once its own page is shown", async () => {
    const first = Promise.withResolvers<undefined>();
    const second = Promise.withResolvers<undefined>();
    h.answers = [
      async () => {
        await first.promise;
        return HttpResponse.json(pageOf([row(1)]));
      },
      async () => {
        await second.promise;
        return HttpResponse.json(pageOf([row(2), row(1)]));
      },
    ];
    start("/messages");
    await until(() => {
      expect(h.asked.length).toBe(1);
    });
    let settled = false;
    const resynced = Promise.all(
      h.watchers.map((w) => w.resync?.(new AbortController().signal)),
    ).then(() => {
      settled = true;
    });
    await until(() => {
      expect(h.asked.length).toBe(2);
    });
    first.resolve(undefined);
    await delay(50);
    expect(settled).toBe(false);
    expect(titles()).toEqual([]);

    second.resolve(undefined);
    await resynced;
    expect(titles()).toEqual(["/workspace/r2", "/workspace/r1"]);
  });

  it("a resync refetches the list before it settles", async () => {
    answer(pageOf([row(1)]), pageOf([row(2), row(1)]));
    start("/messages");
    await rowsShown(["/workspace/r1"]);
    await Promise.all(h.watchers.map((w) => w.resync?.(new AbortController().signal)));
    expect(titles()).toEqual(["/workspace/r2", "/workspace/r1"]);
  });

  it("the filter sheet applies a press at once, counts it on its toggle and clears it", async () => {
    answer(pageOf([row(1)], { counts: { threads: 1, direct: 0 } }));
    start("/messages");
    await rowsShown(["/workspace/r1"]);
    const toggle = button("Filters");
    const dialog = h.root.querySelector<HTMLDialogElement>(
      "dialog#messages-filters",
    ) as HTMLDialogElement;
    toggle.click();
    await until(() => {
      expect(dialog.open).toBe(true);
    });
    expect(document.activeElement?.textContent).toBe("All1 conversation");

    button("Threads1 conversation", dialog).click();
    await until(() => {
      expect(location.search).toBe("?show=threads");
    });
    expect(toggle.textContent).toBe("Filters1 on");
    expect(toggle.nextElementSibling?.textContent).toBe("Clear filters");

    button("Clear all", dialog).click();
    await until(() => {
      expect(location.search).toBe("");
    });
    expect(toggle.textContent).toBe("Filters");
    expect(
      [...(toggle.parentElement?.children ?? [])].some((c) => c.textContent === "Clear filters"),
    ).toBe(false);
    await until(() => {
      expect(list().hasAttribute("aria-busy")).toBe(false);
    });
  });
});
