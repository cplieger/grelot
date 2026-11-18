import type { ChartSpec } from "@cplieger/ui/chart";

import type { Stats } from "../wire/types.gen.js";

/** The words after "in the last" for each range. */
export const rangeWords = { "6h": "6 hours", "24h": "24 hours", "72h": "72 hours" } as const;

const posts = { one: "post", other: "posts" } as const;

/**
 * The series slot of each thread kind, the same on every chart: the kinds
 * posted most take the low slots (path, then direct, then announce, by the
 * grelot_posts_total counts of 2026-11-14).
 */
const kindSlots = { path: 1, direct: 2, announce: 3 } as const;

/** Posts per bucket by thread kind, stacked, on the server's aligned range. */
export function postsSpec(s: Stats, range: keyof typeof rangeWords): ChartSpec {
  const x1 = s.posts.map((b) => b.start_ms);
  return {
    x: { type: "time", domain: [s.start_ms, s.end_ms] },
    y: { label: "Posts", unit: posts },
    marks: [
      {
        mark: "rect",
        x1,
        x2: x1.map((t) => t + s.bucket_ms),
        open: s.posts.at(-1)?.open === true,
        series: [
          { name: "Path", slot: kindSlots.path, y: s.posts.map((b) => b.path) },
          { name: "Direct", slot: kindSlots.direct, y: s.posts.map((b) => b.direct) },
          { name: "Announce", slot: kindSlots.announce, y: s.posts.map((b) => b.announce) },
        ],
      },
    ],
    empty: `No posts in the last ${rangeWords[range]}.`,
  };
}

/**
 * The timeline's rows in the server's order: most posts, then most reads,
 * then name. Two identities that held one name share its row, since the
 * chart refuses a repeated row name, so the order is taken again over the
 * shared rows.
 */
function activityRows(s: Stats): { readonly name: string; readonly x: number[] }[] {
  const rows = new Map<string, { posts: number; read: number; x: number[] }>();
  for (const a of s.agents) {
    const r = rows.get(a.name) ?? { posts: 0, read: 0, x: [] };
    rows.set(a.name, {
      posts: r.posts + a.posts,
      read: r.read + a.read,
      x: [...r.x, ...a.post_times_ms],
    });
  }
  return [...rows]
    .sort(([an, a], [bn, b]) => b.posts - a.posts || b.read - a.read || (an < bn ? -1 : 1))
    .map(([name, r]) => ({ name: `@${name}`, x: r.x.sort((p, q) => p - q) }));
}

/** One row per agent live in the range, most posts first, a dot per post. */
export function activitySpec(s: Stats, range: keyof typeof rangeWords): ChartSpec {
  return {
    x: { type: "time", domain: [s.start_ms, s.end_ms] },
    y: { type: "band", mono: true, unit: { one: "agent", other: "agents" } },
    marks: [
      {
        mark: "dot",
        unit: posts,
        rows: activityRows(s),
      },
    ],
    empty: `No agent posted in the last ${rangeWords[range]}.`,
  };
}

/**
 * Live and active agents each minute, in steps: active is online, heard
 * from in the last 90 seconds. A minute whose store could
 * not be counted is a gap.
 */
export function agentsSpec(s: Stats, range: keyof typeof rangeWords): ChartSpec {
  const counted = (v: number, ok: boolean): number | null => (ok ? v : null);
  return {
    x: { type: "time", domain: [s.start_ms, s.end_ms] },
    y: { label: "Agents", unit: { one: "agent", other: "agents" } },
    marks: [
      {
        mark: "line",
        curve: "step",
        x: s.samples.map((m) => m.at_ms),
        series: [
          { name: "Live", slot: 1, y: s.samples.map((m) => counted(m.live, m.counted)) },
          { name: "Active", slot: 2, y: s.samples.map((m) => counted(m.online, m.counted)) },
        ],
      },
    ],
    empty: `No samples in the last ${rangeWords[range]}.`,
  };
}

/**
 * The read delays on the server's bins, with its median and 95th
 * percentile as rules once anything was read. The summary ends with the
 * deliveries still unread, the same count as the Unread mail figure.
 */
export function delaysSpec(s: Stats, range: keyof typeof rangeWords): ChartSpec {
  const { edges_ms: edges, counts, read } = s.delays;
  const unread = new Intl.NumberFormat().format(s.delays.unread);
  return {
    x: {
      type: "log",
      format: "duration",
      label: "Time from post to read",
      domain: [1_000, edges.at(-1) ?? 1_000],
    },
    y: { label: "Reads", unit: { one: "read", other: "reads" } },
    marks: [
      {
        mark: "rect",
        x1: edges.slice(0, -1),
        x2: edges.slice(1),
        series: [{ name: "Reads", y: counts }],
      },
      ...(read === 0
        ? []
        : [
            { mark: "rule" as const, x: s.delays.median_ms, label: "Median" },
            { mark: "rule" as const, x: s.delays.p95_ms, label: "95th percentile" },
          ]),
    ],
    summary: (generated) => `${generated} ${unread} still unread.`,
    empty: `No post was read in the last ${rangeWords[range]}.`,
  };
}
