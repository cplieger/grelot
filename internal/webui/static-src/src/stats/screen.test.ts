import { delay, http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { userEvent } from "vitest/browser";

import { until } from "../__test-helpers__/until.js";
import {
  button,
  h,
  hour,
  start,
  useMessagesHarness,
  worker,
} from "../messages/__test-helpers__/harness.js";
import type { Sample, Stats } from "../wire/types.gen.js";
import { activitySpec } from "./charts.js";
import { statisticsScreen } from "./screen.js";

useMessagesHarness();

const minute = 60_000;
// The server's 24-hour window: hourly buckets, ending at the end of the hour now is in.
const end = Date.parse("2026-11-13T10:00:00Z");
const startMs = end - 24 * hour;

function sample(over: Partial<Sample> = {}): Sample {
  return {
    at_ms: end - 2 * minute,
    requests: 40,
    errors: 0,
    p95_ms: 12,
    live: 4,
    dormant: 1,
    online: 2,
    unread: 3,
    oldest_unread_ms: 5 * minute,
    held: 1,
    queued: 0,
    in_use_bytes: 1024 * 1024,
    max_bytes: 512 * 1024 * 1024,
    heap_bytes: 8 * 1024 * 1024,
    resident_bytes: 20 * 1024 * 1024,
    goroutines: 30,
    counted: true,
    ...over,
  };
}

const ladder = [0, 1_000, 2_000, 5_000, 10_000, 30_000, minute, 2 * minute, 5 * minute];

function stats(over: Partial<Stats> = {}): Stats {
  return {
    start_ms: startMs,
    end_ms: end,
    bucket_ms: hour,
    started_ms: startMs + hour,
    posts: Array.from({ length: 24 }, (_, i) => ({
      start_ms: startMs + i * hour,
      announce: 0,
      path: i === 22 ? 3 : 0,
      direct: i === 23 ? 1 : 0,
      ...(i === 23 ? { open: true } : {}),
    })),
    agents: [
      {
        name: "alpha",
        post_times_ms: [end - 90 * minute, end - 80 * minute, end - 70 * minute],
        posts: 3,
        read: 2,
        longest_wait_ms: 87_000,
      },
      { name: "bravo", post_times_ms: [end - 30 * minute], posts: 1, read: 0, longest_wait_ms: 0 },
    ],
    threads: [
      {
        thread: { id: 3, kind: "path", title: "/workspace/demo", path: "/workspace/demo" },
        posts: 3,
        participants: 2,
        deliveries: 3,
      },
      {
        thread: { id: 7, kind: "direct", title: "@alpha and @user", agents: ["alpha", "user"] },
        posts: 1,
        participants: 2,
        deliveries: 1,
      },
    ],
    samples: [sample({ at_ms: end - 3 * minute, live: 3 }), sample()],
    events: [
      { kind: "push_gave_up", agent: "bravo", detail: "marotte/notify", at_ms: end - 20 * minute },
      { kind: "push_retry", agent: "bravo", detail: "marotte/notify", at_ms: end - 21 * minute },
    ],
    held_turns: [{ name: "alpha", turns: 4 }],
    delays: {
      edges_ms: ladder,
      counts: [0, 1, 0, 0, 0, 0, 1, 0],
      median_ms: 1_500,
      p95_ms: 87_000,
      read: 2,
      unread: 3,
    },
    event_counts: { post_refused: 0, push_retry: 1, push_gave_up: 1, nudge_repeat: 0 },
    ...over,
  };
}

/** /api/stats answers each in turn, the last one repeating; returns the ranges asked. */
function answerStats(...answers: (() => Response | Promise<Response>)[]): string[] {
  const asked: string[] = [];
  worker.use(
    http.get("/api/stats", ({ request }) => {
      asked.push(new URL(request.url).searchParams.get("range") ?? "");
      const next = answers.length > 1 ? answers.shift() : answers[0];
      return next === undefined ? HttpResponse.json(stats()) : next();
    }),
  );
  return asked;
}

const json = (s: Stats) => () => HttpResponse.json(s);

function open(href = "/statistics"): void {
  start(href, { statistics: statisticsScreen() });
}

function figureNamed(label: string): HTMLElement {
  const f = [...h.root.querySelectorAll<HTMLElement>(".cp-figure")].find(
    (x) => x.querySelector('[data-part="label"]')?.textContent === label,
  );
  if (f === undefined) {
    throw new Error(`no figure ${label}`);
  }
  return f;
}

const valueOf = (label: string): string | null =>
  figureNamed(label).querySelector('[data-part="value"] data')?.getAttribute("value") ?? null;

function chartNamed(title: string): HTMLElement {
  const f = [...h.root.querySelectorAll<HTMLElement>("figure.cp-chart")].find(
    (x) => x.querySelector('[data-part="title"]')?.textContent === title,
  );
  if (f === undefined) {
    throw new Error(`no chart ${title}`);
  }
  return f;
}

const rowsOf = (headingId: string): string[][] =>
  [...h.root.querySelectorAll(`table[aria-labelledby="${headingId}"] tbody tr`)].map((tr) =>
    [...tr.children].map((c) => c.firstElementChild?.textContent ?? c.textContent),
  );

describe("the Statistics tab", () => {
  it("shows the problems, the figures now, the store, the charts and the ranked tables", async () => {
    answerStats(json(stats()));
    open();
    await until(() => {
      expect(valueOf("Live agents")).toBe("4");
    });
    expect(h.root.querySelector("h1")?.textContent).toBe("Statistics");
    expect(valueOf("Active")).toBe("2");
    expect(valueOf("Dormant")).toBe("1");
    expect(valueOf("Unread mail")).toBe("3");
    expect(valueOf("Reservations held")).toBe("1");
    expect(valueOf("Queued")).toBe("0");
    expect(figureNamed("Oldest unread").querySelector('[data-part="value"]')?.textContent).toBe(
      "5\u00A0min",
    );
    expect(figureNamed("Oldest unread").hasAttribute("data-tone")).toBe(false);

    expect(figureNamed("Refused posts").getAttribute("data-tone")).toBe("neutral");
    expect(valueOf("Refused posts")).toBe("0");
    expect(figureNamed("Failed pushes").getAttribute("data-tone")).toBe("warning");
    expect(valueOf("Failed pushes")).toBe("2");
    expect(figureNamed("Repeated nudges").getAttribute("data-tone")).toBe("neutral");

    const meter = h.root.querySelector<HTMLMeterElement>("#stats-store");
    expect(meter?.value).toBe(1024 * 1024);
    expect(meter?.low).toBe(0.8 * 512 * 1024 * 1024);
    expect(meter?.high).toBe(0.9 * 512 * 1024 * 1024);
    expect(h.root.querySelector("#stats-store-words")?.textContent).toBe(
      "1\u00A0MiB of 512\u00A0MiB (0.2%). New writes stop at 90%.",
    );

    for (const title of ["Posts", "Agent activity", "Agents over time"]) {
      expect(chartNamed(title).closest(".cp-chart-stack")).not.toBeNull();
    }
    expect(chartNamed("Time from post to read").closest(".cp-chart-stack")).toBeNull();
    await until(() => {
      expect(
        chartNamed("Time from post to read").querySelector('[data-part="summary"]')?.textContent,
      ).toMatch(/ 3 still unread\.$/);
    });

    expect(rowsOf("stats-agents")).toEqual([
      ["@alpha", "3", "2", "1\u00A0min, 27\u00A0sec", "4"],
      ["@bravo", "1", "0", "No reads", "0"],
    ]);
    expect(
      h.root
        .querySelector<HTMLAnchorElement>('table[aria-labelledby="stats-agents"] a')
        ?.getAttribute("href"),
    ).toBe("/agents/alpha");
    expect(rowsOf("stats-threads")).toEqual([
      ["/workspace/demo", "3", "2", "3"],
      ["You and @alpha", "1", "2", "1"],
    ]);
    expect(
      [
        ...h.root.querySelectorAll<HTMLAnchorElement>('table[aria-labelledby="stats-threads"] a'),
      ].map((a) => a.getAttribute("href")),
    ).toEqual(["/messages/3", "/messages/7"]);
    expect(h.root.querySelector("details summary")?.textContent).toBe("Server");
    expect(h.root.querySelector<HTMLDetailsElement>("details")?.open).toBe(false);
  });

  it("ranks a name two identities held by their posts together on the activity chart", () => {
    const spec = activitySpec(
      stats({
        agents: [
          {
            name: "bravo",
            post_times_ms: [end - 50 * minute],
            posts: 5,
            read: 0,
            longest_wait_ms: 0,
          },
          {
            name: "alpha",
            post_times_ms: [end - 40 * minute],
            posts: 4,
            read: 0,
            longest_wait_ms: 0,
          },
          {
            name: "alpha",
            post_times_ms: [end - 60 * minute],
            posts: 4,
            read: 0,
            longest_wait_ms: 0,
          },
        ],
      }),
      "24h",
    );
    const mark = spec.marks[0];
    expect(mark !== undefined && "rows" in mark ? mark.rows : null).toEqual([
      { name: "@alpha", x: [end - 60 * minute, end - 40 * minute] },
      { name: "@bravo", x: [end - 50 * minute] },
    ]);
  });

  it("marks mail unread for over an hour in the warning tone, in words too", async () => {
    answerStats(json(stats({ samples: [sample({ oldest_unread_ms: hour + minute })] })));
    open();
    await until(() => {
      expect(figureNamed("Oldest unread").getAttribute("data-tone")).toBe("warning");
    });
    expect(figureNamed("Oldest unread").querySelector('[data-part="value"]')?.textContent).toBe(
      "1\u00A0hr, over an hour",
    );
  });

  it("says what is empty when the range holds nothing", async () => {
    answerStats(
      json(
        stats({
          posts: stats().posts.map((b) => ({ ...b, path: 0, direct: 0 })),
          agents: [],
          threads: [],
          samples: [],
          delays: {
            edges_ms: ladder,
            counts: ladder.slice(1).map(() => 0),
            median_ms: 0,
            p95_ms: 0,
            read: 0,
            unread: 0,
          },
          events: [],
          event_counts: { post_refused: 0, push_retry: 0, push_gave_up: 0, nudge_repeat: 0 },
        }),
      ),
    );
    open();
    await until(() => {
      expect(h.root.textContent).toContain("No agent posted or read in the last 24 hours.");
    });
    expect(h.root.textContent).toContain("No thread had a post in the last 24 hours.");
    const none = [...h.root.querySelectorAll<HTMLElement>("p")].find(
      (p) => p.textContent === "The first sample is taken a minute after the server starts.",
    );
    expect(none?.hidden).toBe(false);
    expect(
      h.root.querySelector<HTMLElement>("section[aria-labelledby='stats-now'] .cp-grid")?.hidden,
    ).toBe(true);
    expect(h.root.querySelector<HTMLElement>(".cp-meter")?.hidden).toBe(true);
    await until(() => {
      expect(chartNamed("Posts").querySelector(".cp-empty")?.textContent).toBe(
        "No posts in the last 24 hours.",
      );
    });
    expect(chartNamed("Time from post to read").querySelector(".cp-empty")?.textContent).toBe(
      "No post was read in the last 24 hours.",
    );
    expect(chartNamed("Agents over time").querySelector(".cp-empty")?.textContent).toBe(
      "No samples in the last 24 hours.",
    );
  });

  it("puts a new range in the address and reads it", async () => {
    const asked = answerStats(
      json(stats()),
      json(
        stats({
          agents: [
            { name: "charlie", post_times_ms: [], posts: 0, read: 1, longest_wait_ms: 1_000 },
          ],
        }),
      ),
    );
    open();
    await until(() => {
      expect(rowsOf("stats-agents")).toHaveLength(2);
    });
    h.root.querySelector<HTMLInputElement>('input[value="6h"]')?.click();
    await until(() => {
      expect(rowsOf("stats-agents").map((r) => r[0])).toEqual(["@charlie"]);
    });
    expect(asked).toEqual(["24h", "6h"]);
    expect(location.pathname + location.search).toBe("/statistics?range=6h");
    expect(h.root.querySelector<HTMLInputElement>('input[value="6h"]')?.checked).toBe(true);
  });

  it("offers only the ranges history keeps, moving an address past them to the longest", async () => {
    worker.use(
      http.get("/api/settings", () =>
        HttpResponse.json({
          retention_ms: 12 * hour,
          retention_min_ms: hour,
          retention_max_ms: 2160 * hour,
          methods: { password: true, passkeys: true, oidc: false },
          oidc_configured: false,
        }),
      ),
    );
    const asked = answerStats(json(stats()));
    open("/statistics?range=72h");
    await until(() => {
      expect(location.search).toBe("?range=6h");
    });
    const shown = [...h.root.querySelectorAll<HTMLLabelElement>(".cp-segmented label")]
      .filter((l) => !l.hidden)
      .map((l) => l.textContent);
    expect(shown).toEqual(["6 hours"]);
    await until(() => {
      expect(asked.at(-1)).toBe("6h");
    });
  });

  it("opens a problem's events from its figure, in the address, and closes back to the figure", async () => {
    answerStats(json(stats()));
    open();
    await until(() => {
      expect(valueOf("Failed pushes")).toBe("2");
    });
    const opener = figureNamed("Failed pushes");
    opener.focus();
    opener.click();
    const dialog = h.root.querySelector<HTMLDialogElement>("#stats-problems-push");
    await until(() => {
      expect(dialog?.open).toBe(true);
    });
    expect(location.search).toBe("?problems=push");
    expect(opener.getAttribute("aria-expanded")).toBe("true");
    expect(document.activeElement).toBe(dialog?.querySelector("h2"));
    const rows = [...(dialog?.querySelectorAll(".cp-list-row") ?? [])];
    expect(rows.map((r) => r.querySelector("p")?.textContent)).toEqual([
      "@bravo: Through marotte/notify",
      "@bravo: Through marotte/notify",
    ]);
    expect(rows.map((r) => r.querySelector(".cp-state")?.textContent)).toEqual([
      "Gave up",
      "Retried",
    ]);
    expect(rows.map((r) => r.querySelector(".cp-state")?.getAttribute("data-tone"))).toEqual([
      "danger",
      "warning",
    ]);

    await userEvent.keyboard("{Escape}");
    await until(() => {
      expect(location.search).toBe("");
    });
    expect(opener.getAttribute("aria-expanded")).toBe("false");
    await until(() => {
      expect(document.activeElement).toBe(opener);
    });
  });

  it("opens the problems dialog an address names, saying when nothing went wrong", async () => {
    answerStats(json(stats()));
    open("/statistics?problems=refused");
    const dialog = await until(() => {
      const d = h.root.querySelector<HTMLDialogElement>("#stats-problems-refused");
      expect(d?.open).toBe(true);
      return d;
    });
    expect(dialog?.querySelector(".cp-dialog-body")?.textContent).toBe(
      "No post was refused in the last 24 hours.",
    );
    button("Close", dialog ?? h.root).click();
    await until(() => {
      expect(location.search).toBe("");
    });
  });

  it("says when a load failed and loads again on Retry", async () => {
    answerStats(
      () => HttpResponse.json({ error: "grelot is starting.", code: "timeout" }, { status: 503 }),
      json(stats()),
    );
    open();
    await until(() => {
      expect(h.root.querySelector(".cp-notice[data-tone='danger']")?.textContent).toContain(
        "The statistics could not be loaded. grelot is starting.",
      );
    });
    button("Retry").click();
    await until(() => {
      expect(valueOf("Live agents")).toBe("4");
    });
    expect(h.root.querySelector(".cp-notice[data-tone='danger']")).toBeNull();
  });

  it("reads again each minute, rolling the figures to the new numbers", async () => {
    answerStats(json(stats()), async () => {
      await delay(10);
      return HttpResponse.json(stats({ samples: [sample({ live: 6 })] }));
    });
    start("/statistics", { statistics: statisticsScreen({ refreshMs: 50 }) });
    await until(() => {
      expect(valueOf("Live agents")).toBe("4");
    });
    await until(() => {
      expect(valueOf("Live agents")).toBe("6");
    });
  });
});
