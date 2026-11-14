import { delay, http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { until } from "../__test-helpers__/until.js";
import type {
  Agent,
  AgentPage,
  HistoryPage,
  HistoryPost,
  Reservation,
  Thread,
} from "../wire/types.gen.js";
import {
  at,
  button,
  emit,
  h,
  hour,
  rested,
  start,
  useMessagesHarness,
  worker,
} from "./__test-helpers__/harness.js";

useMessagesHarness();

const minute = 60_000;

function agent(over: Partial<Agent> = {}): Agent {
  return {
    name: "alpha",
    description: "",
    state: "live",
    paths: [],
    follows: [],
    registered_ms: at - 5 * hour,
    ...over,
  };
}

function pageOf(over: Partial<AgentPage> = {}, card: Partial<Agent> = {}): AgentPage {
  return { agent: agent(card), reservations: [], delegates: [], ...over };
}

function reservation(over: Partial<Reservation> = {}): Reservation {
  return {
    id: 1,
    path: "/workspace/demo",
    holder: "alpha",
    state: "held",
    since_ms: at - hour,
    expires_ms: at + hour,
    ...over,
  };
}

const direct: Thread = {
  id: 7,
  kind: "direct",
  title: "@alpha and @user",
  agents: ["alpha", "user"],
};
const demo: Thread = { id: 3, kind: "path", title: "/workspace/demo", path: "/workspace/demo" };

function historyPost(id: number, over: Partial<HistoryPost> = {}): HistoryPost {
  return {
    id,
    thread: 7,
    from: "alpha",
    from_kind: "agent",
    subject: `s${String(id)}`,
    body: `b${String(id)}`,
    read: { read: 0, total: 1 },
    at_ms: at - (10 - id) * minute,
    conversation: direct,
    ...over,
  };
}

/** /api/agents/alpha answers each in turn, the last one repeating; returns the request count. */
function card(...answers: (AgentPage | (() => Response | Promise<Response>))[]): { n: number } {
  const count = { n: 0 };
  worker.use(
    http.get("/api/agents/alpha", () => {
      count.n++;
      const next = answers.length > 1 ? answers.shift() : answers[0];
      return typeof next === "function" ? next() : HttpResponse.json(next);
    }),
  );
  return count;
}

/** /api/agents/alpha/history answers by its before; returns each query asked. */
function history(
  pages: Readonly<Record<string, HistoryPage | (() => Response | Promise<Response>)>>,
): URLSearchParams[] {
  const asked: URLSearchParams[] = [];
  worker.use(
    http.get("/api/agents/alpha/history", ({ request }) => {
      const q = new URL(request.url).searchParams;
      asked.push(q);
      const page = pages[q.get("before") ?? "newest"];
      if (page === undefined) {
        return HttpResponse.json({ error: "no such page", code: "invalid" }, { status: 400 });
      }
      return typeof page === "function" ? page() : HttpResponse.json(page);
    }),
  );
  return asked;
}

const busy = (): Response =>
  HttpResponse.json({ error: "The store is busy.", code: "timeout" }, { status: 503 });

const main = (): HTMLElement => h.root.querySelector("main") as HTMLElement;
const facts = (): Record<string, HTMLElement> =>
  Object.fromEntries(
    [...main().querySelectorAll("dl.cp-facts > dt")].map((dt) => [
      dt.textContent,
      dt.nextElementSibling as HTMLElement,
    ]),
  );
const entries = (): HTMLElement[] => [
  ...main().querySelectorAll<HTMLElement>("li.cp-feed-entry:not([data-skeleton])"),
];
const subjects = (): string[] => entries().map((e) => e.querySelector("h3")?.textContent ?? "");
const stateLine = (): HTMLElement => main().querySelector(".cp-heading-row p") as HTMLElement;

async function loaded(): Promise<void> {
  await until(() => {
    expect(main().querySelector("dl.cp-facts")).not.toBeNull();
  });
}

async function shown(n: number): Promise<void> {
  await until(() => {
    expect(entries().length).toBe(n);
  });
}

describe("the agent page", () => {
  it("heads the page with the name, its way back to Messages, its state and Message @name", async () => {
    card(pageOf({}, { online: true }));
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    expect(main().querySelector("h1")?.textContent).toBe("@alpha");
    expect(document.title).toBe("@alpha · grelot");
    expect(h.root.querySelector('.cp-nav a[href="/messages"]')?.getAttribute("aria-current")).toBe(
      "true",
    );
    const crumbs = main().querySelector("nav.cp-crumbs") as HTMLElement;
    expect(crumbs.getAttribute("aria-label")).toBe("Breadcrumb");
    expect(crumbs.querySelector("a")?.getAttribute("href")).toBe("/messages");
    expect(crumbs.querySelector("a")?.textContent).toBe("Messages");
    expect(crumbs.querySelector('[aria-current="page"]')?.textContent).toBe("@alpha");

    await loaded();
    const live = stateLine().querySelector(".cp-state") as HTMLElement;
    expect(live.textContent).toBe("Live");
    expect(live.getAttribute("data-tone")).toBe("success");
    expect(stateLine().querySelector(".cp-dot")?.hasAttribute("data-live")).toBe(true);
    expect(stateLine().textContent).toContain("Online");
    expect(button("Message @alpha", main()).hidden).toBe(false);
  });

  it("shows each fact the card has, and leaves out the ones it lacks", async () => {
    card(
      pageOf(
        { delegates: ["alpha/review"] },
        {
          description: "Builds the web UI",
          host: "borgcube",
          provider: "kiro-cli",
          paths: ["/workspace/grelot", "/workspace/ui"],
          follows: ["/workspace/docs"],
          last_seen_ms: at - 2 * minute,
          expires_ms: at + 3 * hour,
        },
      ),
    );
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await loaded();
    const f = facts();
    expect(Object.keys(f)).toEqual([
      "Description",
      "Machine",
      "Registered",
      "Last seen",
      "Lease ends",
      "Paths",
      "Follows",
      "Delegates",
    ]);
    expect(f["Description"]?.textContent).toBe("Builds the web UI");
    expect(f["Machine"]?.textContent).toBe("borgcube (kiro-cli)");
    expect(f["Registered"]?.querySelector("time")?.getAttribute("datetime")).toBe(
      new Date(at - 5 * hour).toISOString(),
    );
    expect(f["Last seen"]?.querySelector("time")?.getAttribute("datetime")).toBe(
      new Date(at - 2 * minute).toISOString(),
    );
    expect(f["Lease ends"]?.querySelector("time")?.getAttribute("datetime")).toBe(
      new Date(at + 3 * hour).toISOString(),
    );
    const paths = [...(f["Paths"]?.querySelectorAll("li") ?? [])];
    expect(paths.map((li) => li.textContent)).toEqual(["/workspace/grelot", "/workspace/ui"]);
    expect(paths.every((li) => li.classList.contains("cp-mono"))).toBe(true);
    expect(f["Follows"]?.textContent).toBe("/workspace/docs");
    expect(f["Delegates"]?.textContent).toBe("@alpha/review");
    expect(f["Delegates"]?.querySelector("a")).toBeNull();
    expect(stateLine().querySelector(".cp-dot")).toBeNull();
    expect(stateLine().textContent).not.toContain("Online");
  });

  it("says a dormant agent is dormant and since when, in place of its lease", async () => {
    card(pageOf({}, { state: "dormant", dormant_since_ms: at - hour }));
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await loaded();
    expect(Object.keys(facts())).toEqual(["Registered", "Dormant since"]);
    const dormant = stateLine().querySelector(".cp-state") as HTMLElement;
    expect(dormant.textContent).toBe("Dormant");
    expect(dormant.getAttribute("data-tone")).toBe("neutral");
  });

  it("lists its reservations in a table whose Until and Reason leave first on a narrow box", async () => {
    card(
      pageOf({
        reservations: [
          reservation({ reason: "fixing the feed" }),
          reservation({
            id: 2,
            path: "/workspace/ui",
            state: "queued",
            place: 2,
            expires_ms: at + 2 * hour,
          }),
        ],
      }),
    );
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await loaded();
    const table = main().querySelector("table.cp-table") as HTMLTableElement;
    const label = document.getElementById(table.getAttribute("aria-labelledby") ?? "");
    expect(label?.textContent).toBe("Reservations");
    expect(
      [...table.tHead!.rows[0]!.cells].map((c) => [c.textContent, c.dataset["priority"]]),
    ).toEqual([
      ["Path", undefined],
      ["State", undefined],
      ["Until", "mid"],
      ["Reason", "low"],
    ]);
    const rows = [...table.tBodies[0]!.rows].map((r) => [...r.cells]);
    expect(rows.map((cells) => cells.map((c) => c.dataset["priority"]))).toEqual([
      [undefined, undefined, "mid", "low"],
      [undefined, undefined, "mid", "low"],
    ]);
    expect(rows[0]?.[0]?.tagName).toBe("TH");
    expect(rows[0]?.[0]?.getAttribute("scope")).toBe("row");
    expect(rows[0]?.[0]?.querySelector(".cp-mono")?.textContent).toBe("/workspace/demo");
    expect(rows[0]?.[1]?.textContent).toBe("Held");
    expect(rows[1]?.[1]?.textContent).toBe("Queued, 2nd");
    expect(rows[0]?.[2]?.querySelector("time")?.getAttribute("datetime")).toBe(
      new Date(at + hour).toISOString(),
    );
    expect(rows[0]?.[3]?.textContent).toBe("fixing the feed");
    expect(rows[1]?.[3]?.textContent).toBe("");
  });

  it("says when it holds no reservations", async () => {
    card(pageOf());
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await loaded();
    expect(main().querySelector("table")).toBeNull();
    expect(main().textContent).toContain("Holds no reservations.");
  });

  it("says when no agent has the name, with a way back to Messages and no Message button", async () => {
    card(() =>
      HttpResponse.json({ error: "no agent is called @alpha", code: "not_found" }, { status: 404 }),
    );
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await until(() => {
      expect(main().querySelector(".cp-empty strong")?.textContent).toBe("No agent called @alpha.");
    });
    const back = main().querySelector(".cp-empty a.cp-button") as HTMLAnchorElement;
    expect(back.textContent).toBe("Back to messages");
    expect(back.getAttribute("href")).toBe("/messages");
    expect(button("Message @alpha", main()).hidden).toBe(true);
    expect(stateLine().textContent).toBe("");
  });

  it("turns to the empty state when the agent leaves while its page is open", async () => {
    card(pageOf(), () =>
      HttpResponse.json({ error: "no agent is called @alpha", code: "not_found" }, { status: 404 }),
    );
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await loaded();
    expect(button("Message @alpha", main()).hidden).toBe(false);
    emit({ conversations: [], agents: true });
    await until(() => {
      expect(main().querySelector(".cp-empty strong")?.textContent).toBe("No agent called @alpha.");
    });
    expect(main().querySelector("dl.cp-facts")).toBeNull();
    expect(button("Message @alpha", main()).hidden).toBe(true);
    expect(stateLine().textContent).toBe("");
  });

  it("shows the server's words when the card fails, and Retry loads it", async () => {
    const asked = card(busy, pageOf());
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await until(() => {
      expect(
        main().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')?.textContent,
      ).toContain("@alpha could not be loaded. The store is busy.");
    });
    expect(button("Message @alpha", main()).hidden).toBe(true);
    button("Retry", main()).click();
    await loaded();
    expect(asked.n).toBe(2);
    expect(main().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
  });

  it("lists what it sent and received newest first, each naming its conversation as a link", async () => {
    card(pageOf());
    history({
      newest: {
        posts: [
          historyPost(9, { from: "user", from_kind: "user", read: { read: 1, total: 1 } }),
          historyPost(8, { conversation: demo, thread: 3, read: { read: 0, total: 0 } }),
          historyPost(2, { at_ms: at - 2 * 24 * hour }),
        ],
      },
    });
    start("/agents/alpha");
    await shown(3);
    expect(main().querySelector("section h2:not([id])")?.textContent).toBe("History");
    expect(subjects()).toEqual(["s9", "s8", "s2"]);
    const [own, path, old] = entries();
    expect(own?.hasAttribute("data-own")).toBe(true);
    expect(own?.querySelector("header")?.firstElementChild?.textContent).toBe("You");
    const chip = (e: HTMLElement | undefined): HTMLAnchorElement | null | undefined =>
      e?.querySelector(".cp-chip a");
    expect(chip(own)?.textContent).toBe("You and @alpha");
    expect(chip(own)?.getAttribute("href")).toBe("/messages/7");
    expect(chip(own)?.classList.contains("cp-mono")).toBe(false);
    expect(chip(path)?.textContent).toBe("/workspace/demo");
    expect(chip(path)?.getAttribute("href")).toBe("/messages/3");
    expect(chip(path)?.classList.contains("cp-mono")).toBe(true);
    expect(path?.querySelector("header a")?.getAttribute("href")).toBe("/agents/alpha");
    expect(own?.querySelector("footer")?.textContent).toBe("Read by 1 of 1");
    expect(path?.querySelector("footer")?.textContent).toBe("Reached no one.");
    const days = [...main().querySelectorAll("li.cp-feed-day")];
    expect(days.length).toBe(2);
    expect(days[1]?.nextElementSibling).toBe(old);
    expect(main().querySelector(".cp-feed")?.nextElementSibling).toBeNull();
  });

  it("says when it has no messages, in the retention's words", async () => {
    card(pageOf());
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await until(() => {
      expect(main().querySelector("section .cp-empty")?.textContent).toBe(
        "No messages in the last 72 hours.",
      );
    });
  });

  it("Show older adds the next page after the oldest, then leaves for its first message", async () => {
    card(pageOf());
    const asked = history({
      newest: { posts: [historyPost(9), historyPost(8)], next: 8 },
      "8": { posts: [historyPost(5), historyPost(4)] },
    });
    start("/agents/alpha");
    await shown(2);
    const older = await rested("Show older", main());
    const foot = older.closest(".cp-list-foot");
    expect(foot?.tagName).toBe("DIV");
    expect(main().querySelector(".cp-feed")?.nextElementSibling).toBe(foot);
    older.focus();
    older.click();
    await shown(4);
    expect(subjects()).toEqual(["s9", "s8", "s5", "s4"]);
    expect(asked.at(-1)?.get("before")).toBe("8");
    await until(() => {
      expect(older.isConnected).toBe(false);
    });
    expect(document.activeElement).toBe(entries()[2]);
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "2 older messages",
      );
    });
  });

  it("says a failed Show older, keeps what it shows, and its button reads Retry", async () => {
    card(pageOf());
    history({ newest: { posts: [historyPost(9)], next: 9 }, "9": busy });
    start("/agents/alpha");
    await shown(1);
    (await rested("Show older", main())).click();
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "Older messages could not be loaded.",
      );
    });
    expect(subjects()).toEqual(["s9"]);
    expect((await rested("Retry", main())).closest(".cp-list-foot")).not.toBeNull();
  });

  it("shows the server's words when the history fails, and Retry loads it", async () => {
    card(pageOf());
    let first = true;
    history({
      newest: () => {
        if (first) {
          first = false;
          return busy();
        }
        return HttpResponse.json({ posts: [historyPost(9)] });
      },
    });
    start("/agents/alpha");
    await until(() => {
      expect(main().querySelector('section .cp-notice[data-tone="danger"]')?.textContent).toContain(
        "History could not be loaded. The store is busy.",
      );
    });
    button("Retry", main()).click();
    await shown(1);
    expect(main().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
  });

  it("loads a failed history again when the stream reports a change to a conversation", async () => {
    card(pageOf());
    let first = true;
    const asked = history({
      newest: () => {
        if (first) {
          first = false;
          return busy();
        }
        return HttpResponse.json({ posts: [historyPost(9)] });
      },
    });
    start("/agents/alpha");
    await until(() => {
      expect(main().querySelector('section .cp-notice[data-tone="danger"]')?.textContent).toContain(
        "History could not be loaded.",
      );
    });
    emit({ conversations: [7], agents: false });
    await shown(1);
    expect(asked.length).toBe(2);
    expect(main().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
  });

  it("draws the history a returning card loads, over a refresh it replaced that fails late", async () => {
    const gone = (): Response =>
      HttpResponse.json({ error: "no agent is called @alpha", code: "not_found" }, { status: 404 });
    card(pageOf(), gone, pageOf());
    const held = Promise.withResolvers<undefined>();
    let call = 0;
    history({
      newest: async () => {
        call++;
        if (call === 1) {
          return HttpResponse.json({ posts: [historyPost(9)] });
        }
        if (call === 2) {
          await held.promise;
          return busy();
        }
        return HttpResponse.json({ posts: [historyPost(10), historyPost(9)] });
      },
    });
    start("/agents/alpha");
    await shown(1);
    emit({ conversations: [7], agents: false });
    await until(() => {
      expect(call).toBe(2);
    });
    emit({ conversations: [], agents: true });
    await until(() => {
      expect(main().querySelector(".cp-empty strong")?.textContent).toBe("No agent called @alpha.");
    });
    emit({ conversations: [], agents: true });
    await shown(2);
    held.resolve(undefined);
    await delay(50);
    expect(subjects()).toEqual(["s10", "s9"]);
    expect(main().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
  });

  it("follows the live stream: an agents change redraws only what moved, a post folds in on top", async () => {
    const asked = card(
      pageOf({}, { description: "first" }),
      pageOf({ reservations: [reservation()] }, { description: "first" }),
    );
    let newest: HistoryPage = { posts: [historyPost(9), historyPost(8)], next: 8 };
    const queries = history({
      newest: () => HttpResponse.json(newest),
      "8": { posts: [historyPost(5)], next: 5 },
      "5": { posts: [historyPost(4)] },
    });
    start("/agents/alpha");
    await shown(2);
    (await rested("Show older", main())).click();
    await shown(3);
    const panel = main().querySelector("dl.cp-facts");

    emit({ conversations: [], agents: true });
    await until(() => {
      expect(main().querySelector("table.cp-table")).not.toBeNull();
    });
    expect(asked.n).toBe(2);
    expect(main().querySelector("dl.cp-facts")).toBe(panel);

    const kept = entries()[2];
    newest = { posts: [historyPost(10), historyPost(9), historyPost(8)], next: 8 };
    emit({ conversations: [7], agents: false });
    await shown(4);
    expect(subjects()).toEqual(["s10", "s9", "s8", "s5"]);
    expect(entries()[3]).toBe(kept);
    (await rested("Show older", main())).click();
    await shown(5);
    expect(queries.at(-1)?.get("before")).toBe("5");
  });

  it("a refresh reads the older pages again: an expired message leaves, a changed one is redrawn in place", async () => {
    card(pageOf());
    let changed = false;
    const asked = history({
      newest: { posts: [historyPost(9), historyPost(8)], next: 8 },
      "8": () =>
        HttpResponse.json(
          changed
            ? { posts: [historyPost(4, { read: { read: 1, total: 1 } }), historyPost(3)], next: 3 }
            : { posts: [historyPost(5), historyPost(4)], next: 4 },
        ),
      "4": { posts: [historyPost(3)] },
    });
    start("/agents/alpha");
    await shown(2);
    (await rested("Show older", main())).click();
    await shown(4);
    const fourth = entries()[3];
    expect(fourth?.querySelector("footer")?.textContent).toBe("Read by 0 of 1");

    changed = true;
    emit({ conversations: [7], agents: false });
    await until(() => {
      expect(subjects()).toEqual(["s9", "s8", "s4"]);
    });
    expect(entries()[2]).toBe(fourth);
    expect(fourth?.querySelector("footer")?.textContent).toBe("Read by 1 of 1");
    expect(asked.at(-1)?.get("before")).toBe("8");

    (await rested("Show older", main())).click();
    await shown(4);
    expect(asked.at(-1)?.get("before")).toBe("4");
  });

  it("fetches a post that lands while the first history page loads, once that page is shown", async () => {
    card(pageOf());
    const held = Promise.withResolvers<undefined>();
    let newest: HistoryPage = { posts: [historyPost(9)] };
    const asked = history({
      newest: async () => {
        const page = newest;
        await held.promise;
        return HttpResponse.json(page);
      },
    });
    start("/agents/alpha");
    await until(() => {
      expect(asked.length).toBe(1);
    });
    newest = { posts: [historyPost(10), historyPost(9)] };
    emit({ conversations: [7], agents: false });
    held.resolve(undefined);
    await shown(2);
    expect(subjects()).toEqual(["s10", "s9"]);
  });

  it("draws a change that lands while the card first loads, and keeps it over the older answer", async () => {
    const held = Promise.withResolvers<undefined>();
    const asked = card(
      async () => {
        await held.promise;
        return HttpResponse.json(pageOf({}, { description: "first" }));
      },
      pageOf({}, { description: "second" }),
    );
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await until(() => {
      expect(main().querySelector('div[aria-busy="true"]')).not.toBeNull();
    });
    emit({ conversations: [], agents: true });
    held.resolve(undefined);
    await until(() => {
      expect(main().querySelector("div[aria-busy]")).toBeNull();
      expect(facts()["Description"]?.textContent).toBe("second");
    });
    expect(asked.n).toBe(2);
  });

  it("draws a card refresh that finishes while more agents changes arrive, then reads once more for them", async () => {
    const second = Promise.withResolvers<undefined>();
    const third = Promise.withResolvers<undefined>();
    const asked = card(
      pageOf({}, { description: "first" }),
      async () => {
        await second.promise;
        return HttpResponse.json(pageOf({}, { description: "second" }));
      },
      async () => {
        await third.promise;
        return HttpResponse.json(pageOf({}, { description: "third" }));
      },
      pageOf({}, { description: "fourth" }),
    );
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await loaded();
    expect(facts()["Description"]?.textContent).toBe("first");

    emit({ conversations: [], agents: true });
    await until(() => {
      expect(asked.n).toBe(2);
    });
    emit({ conversations: [], agents: true });
    emit({ conversations: [], agents: true });
    second.resolve(undefined);
    await until(() => {
      expect(facts()["Description"]?.textContent).toBe("second");
    });
    await until(() => {
      expect(asked.n).toBe(3);
    });
    third.resolve(undefined);
    await until(() => {
      expect(facts()["Description"]?.textContent).toBe("third");
    });

    emit({ conversations: [], agents: true });
    await until(() => {
      expect(facts()["Description"]?.textContent).toBe("fourth");
    });
    expect(asked.n).toBe(4);
  });

  it("a resync during the card's first load replaces it, and settles once its own card is shown", async () => {
    const first = Promise.withResolvers<undefined>();
    const second = Promise.withResolvers<undefined>();
    const asked = card(
      async () => {
        await first.promise;
        return HttpResponse.json(pageOf({}, { description: "first" }));
      },
      async () => {
        await second.promise;
        return HttpResponse.json(pageOf({}, { description: "second" }));
      },
    );
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await until(() => {
      expect(asked.n).toBe(1);
    });
    let settled = false;
    const resynced = Promise.all(
      h.watchers.map((w) => w.resync?.(new AbortController().signal)),
    ).then(() => {
      settled = true;
    });
    await until(() => {
      expect(asked.n).toBe(2);
    });
    first.resolve(undefined);
    await delay(50);
    expect(settled).toBe(false);
    expect(main().querySelector("dl.cp-facts")).toBeNull();

    second.resolve(undefined);
    await resynced;
    expect(facts()["Description"]?.textContent).toBe("second");
  });

  it("keeps the card under a notice when a refresh fails, and its Retry refreshes it", async () => {
    card(pageOf({}, { description: "first" }), busy, pageOf({}, { description: "second" }));
    history({ newest: { posts: [] } });
    start("/agents/alpha");
    await loaded();

    emit({ conversations: [], agents: true });
    await until(() => {
      expect(
        main().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')?.textContent,
      ).toBe("@alpha could not be refreshed. The store is busy.Retry");
    });
    expect(facts()["Description"]?.textContent).toBe("first");
    button("Retry", main()).click();
    await until(() => {
      expect(facts()["Description"]?.textContent).toBe("second");
    });
    expect(main().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
  });

  it("keeps the history under a notice when a refresh fails", async () => {
    card(pageOf());
    let fail = false;
    history({
      newest: () => (fail ? busy() : HttpResponse.json({ posts: [historyPost(9)] })),
    });
    start("/agents/alpha");
    await shown(1);
    fail = true;
    emit({ conversations: [7], agents: false });
    await until(() => {
      expect(main().querySelector('section .cp-notice[data-tone="danger"]')?.textContent).toContain(
        "History could not be refreshed. The store is busy.",
      );
    });
    expect(subjects()).toEqual(["s9"]);
    fail = false;
    button("Retry", main()).click();
    await until(() => {
      expect(main().querySelector('.cp-notice[data-tone="danger"]:not([hidden])')).toBeNull();
    });
  });

  it("Message @name opens the New message dialog to it, and a send opens the conversation", async () => {
    card(pageOf());
    history({ newest: { posts: [] } });
    worker.use(
      http.post("/api/posts", () => HttpResponse.json({ message: 70, thread: 7, delivered: 1 })),
    );
    start("/agents/alpha");
    await loaded();
    const write = button("Message @alpha", main());
    write.focus();
    write.click();
    const dialog = document.getElementById("new-message") as HTMLDialogElement;
    await until(() => {
      expect(dialog.open).toBe(true);
    });
    expect((document.getElementById("new-message-to") as HTMLInputElement).value).toBe("@alpha");
    expect(document.activeElement).toBe(document.getElementById("new-message-subject"));
    (document.getElementById("new-message-subject") as HTMLInputElement).value = "Hello";
    button("Send", dialog).click();
    await until(() => {
      expect(location.pathname).toBe("/messages/7");
    });
  });
});

describe("the user's own page", () => {
  it("offers no Message button, since the user cannot write to @user", async () => {
    worker.use(
      http.get("/api/agents/user", () => HttpResponse.json(pageOf({}, { name: "user" }))),
      http.get("/api/agents/user/history", () => HttpResponse.json({ posts: [] })),
    );
    start("/agents/user");
    await loaded();
    expect(button("Message @user", main()).hidden).toBe(true);
    await until(() => {
      expect(main().textContent).toContain("No messages in the last 72 hours.");
    });
  });
});

describe("the agent page under a right-to-left page", () => {
  const direction = (n: Element | null | undefined): string =>
    n === null || n === undefined ? "missing" : getComputedStyle(n).direction;

  it("keeps the name, the card's facts, the reservations and the history in their own direction", async () => {
    document.documentElement.dir = "rtl";
    try {
      card(
        pageOf(
          { reservations: [reservation({ reason: "fixing the feed" })] },
          { description: "Builds the web UI", host: "borgcube", paths: ["/workspace/grelot"] },
        ),
      );
      history({ newest: { posts: [historyPost(1)] } });
      start("/agents/alpha");
      await loaded();
      await shown(1);
      const f = facts();
      const cells = main().querySelectorAll("table.cp-table tbody tr > *");
      const [entry] = entries();

      expect({
        name: direction(main().querySelector("h1")),
        crumb: direction(main().querySelector('nav.cp-crumbs [aria-current="page"]')),
        description: direction(f["Description"]?.firstElementChild),
        machine: direction(f["Machine"]?.firstElementChild),
        path: direction(f["Paths"]?.querySelector("li")),
        reservedPath: direction(cells[0]?.querySelector("code")),
        reason: direction(cells[3]),
        subject: direction(entry?.querySelector("h3")),
        body: direction(entry?.querySelector(".cp-prose")),
      }).toEqual({
        name: "ltr",
        crumb: "ltr",
        description: "ltr",
        machine: "ltr",
        path: "ltr",
        reservedPath: "ltr",
        reason: "ltr",
        subject: "ltr",
        body: "ltr",
      });
      expect(direction(main())).toBe("rtl");
    } finally {
      document.documentElement.removeAttribute("dir");
    }
  });
  it("keeps the name in a failed card's notice in its own direction", async () => {
    document.documentElement.dir = "rtl";
    try {
      card(busy);
      history({ newest: { posts: [] } });
      start("/agents/alpha");
      await until(() => {
        expect(main().querySelector('.cp-notice[data-tone="danger"] p')).not.toBeNull();
      });
      const words = main().querySelector('.cp-notice[data-tone="danger"] p');
      expect(words?.textContent).toBe("@alpha could not be loaded. The store is busy.");
      expect(direction(words?.querySelector("bdi"))).toBe("ltr");
    } finally {
      document.documentElement.removeAttribute("dir");
    }
  });
});
