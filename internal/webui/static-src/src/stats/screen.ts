import { el } from "@cplieger/reactive/dom";
import { renderChart, type Chart, type ChartSpec } from "@cplieger/ui/chart";
import { enhanceDisclosure } from "@cplieger/ui/disclosure";
import { enhancePill } from "@cplieger/ui/pill";

import { clockTime } from "../format.js";
import { icon } from "../icons.js";
import { reader, type Answer } from "../messages/reader.js";
import { retryButton } from "../messages/parts.js";
import type { ScreenMount } from "../shell.js";
import { failureWords, type ApiResult } from "../transport.js";
import { getSettings, getStats } from "../wire/client.gen.js";
import type { Stats, StatsRange } from "../wire/types.gen.js";
import { activitySpec, agentsSpec, delaysSpec, postsSpec, rangeWords } from "./charts.js";
import { nowFigures, serverFigures } from "./figures.js";
import { needsAttention } from "./problems.js";
import { agentsTable, threadsTable } from "./tables.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

const ranges: readonly StatsRange[] = ["6h", "24h", "72h"];
const rangeMs: Readonly<Record<StatsRange, number>> = {
  "6h": 6 * 3_600_000,
  "24h": 24 * 3_600_000,
  "72h": 72 * 3_600_000,
};

/** The ranges a retention offers: those it keeps whole, and 6 hours at least. */
function offeredRanges(retentionMs: number): readonly StatsRange[] {
  const kept = ranges.filter((r) => rangeMs[r] <= retentionMs);
  return kept.length === 0 ? ["6h"] : kept;
}

function notice(words: string, retry: () => void): HTMLElement {
  return el(
    "div",
    { className: "cp-notice", "data-tone": "danger" },
    icon("circle-x"),
    el("p", null, words),
    retryButton(retry),
  );
}

/** A chart's figure with its caption and its Table view chip, and its handle. */
function chartFigure(
  caption: string,
  signal: AbortSignal,
): { readonly figure: HTMLElement; readonly chart: Chart } {
  const toggle = el(
    "button",
    { className: "cp-filter-chip", type: "button", "aria-pressed": "false", hidden: true },
    icon("check"),
    "Table view",
  ) as HTMLButtonElement;
  const figure = el(
    "figure",
    { className: "cp-chart" },
    el("figcaption", null, caption),
    el("div", { className: "cp-toolbar" }, toggle),
  );
  return { figure, chart: renderChart(figure, null, { signal, toggle }) };
}

/** What a section shows until its first answer: skeleton lines once a slow load shows them. */
interface Loadable {
  readonly content: HTMLElement;
  readonly skeleton: HTMLElement;
}

function loadable(content: HTMLElement, lines: number): Loadable {
  content.hidden = true;
  const skeleton = el(
    "div",
    { className: "cp-stack", hidden: true },
    ...Array.from({ length: lines }, () => el("span", { className: "cp-skeleton" })),
  );
  return { content, skeleton };
}

function section(id: string, heading: string, part: Loadable): HTMLElement {
  return el(
    "section",
    { "aria-labelledby": id },
    el("h2", { id }, heading),
    part.skeleton,
    part.content,
  );
}

interface StatisticsOptions {
  /** How often the open tab reads the statistics again, which the server samples each minute. */
  readonly refreshMs?: number;
}

/**
 * The Statistics tab (shape 3): the range in the address, the delivery
 * problems, the figures now, the four charts, the ranked tables and the
 * collapsed Server section, read again each minute.
 */
export function statisticsScreen(opts: StatisticsOptions = {}): ScreenMount<"statistics"> {
  const refreshMs = opts.refreshMs ?? 60_000;
  return (host, first, ctx) => {
    const { signal, router, live } = ctx;
    let route = first;

    const choices = new Map<StatsRange, HTMLLabelElement>();
    const segmented = el(
      "div",
      { className: "cp-segmented", role: "radiogroup", "aria-label": "Range" },
      ...ranges.map((r) => {
        const input = el("input", {
          type: "radio",
          name: "stats-range",
          value: r,
          checked: r === route.range,
        });
        const label = el("label", null, input, rangeWords[r]) as HTMLLabelElement;
        choices.set(r, label);
        return label;
      }),
    );
    segmented.addEventListener(
      "change",
      (e) => {
        const value = (e.target as HTMLInputElement).value as StatsRange;
        router.go({ ...route, range: value }, { replace: true });
      },
      { signal },
    );

    const attention = needsAttention({
      signal,
      onOpen: (which) => {
        router.go({ ...route, problems: which }, { replace: true });
      },
      onClose: () => {
        router.go({ ...route, problems: null }, { replace: true });
      },
    });
    const now = nowFigures(signal);
    const posts = chartFigure("Posts", signal);
    const activity = chartFigure("Agent activity", signal);
    const agents = chartFigure("Agents over time", signal);
    const delays = chartFigure("Time from post to read", signal);
    const charts: readonly (readonly [Chart, (s: Stats, r: StatsRange) => ChartSpec])[] = [
      [posts.chart, postsSpec],
      [activity.chart, activitySpec],
      [agents.chart, agentsSpec],
      [delays.chart, delaysSpec],
    ];
    const emptyAgents = el("p", { className: "cp-meta" });
    const emptyThreads = el("p", { className: "cp-meta" });
    const who = agentsTable(signal, emptyAgents);
    const threads = threadsTable(signal, emptyThreads);
    const server = serverFigures();
    const details = el(
      "details",
      null,
      el("summary", null, "Server"),
      server.element,
    ) as HTMLDetailsElement;
    enhanceDisclosure(details, { signal });
    const started = el("p", { className: "cp-meta" });

    const parts = {
      attention: loadable(attention.element, 1),
      now: loadable(now.element, 3),
      who: loadable(who.element, 4),
      threads: loadable(threads.element, 4),
      started: loadable(started, 1),
    };
    const sections = el(
      "div",
      { className: "cp-stack" },
      section("stats-attention", "Needs attention", parts.attention),
      section("stats-now", "Now", parts.now),
      el("div", { className: "cp-chart-stack" }, posts.figure, activity.figure, agents.figure),
      delays.figure,
      section("stats-agents", "Who posts and reads", parts.who),
      section("stats-threads", "Threads", parts.threads),
      details,
      parts.started.skeleton,
      started,
    );
    /** Shows the answer's parts (true), or hides them for a load, with their skeletons (null) or without. */
    const showParts = (on: boolean | null): void => {
      for (const p of Object.values(parts)) {
        p.content.hidden = on !== true;
        p.skeleton.hidden = on !== null;
      }
    };
    const failedNotice = el("div");
    const staleNotice = el("div");
    host.append(
      el(
        "div",
        { className: "cp-heading-row" },
        el("h1", null, "Statistics"),
        el("div", { className: "cp-toolbar" }, segmented),
      ),
      failedNotice,
      staleNotice,
      sections,
      attention.dialogs,
    );
    enhancePill(segmented, { signal });

    let shown = false;
    const draw = (s: Stats, r: StatsRange): void => {
      const words = rangeWords[r];
      sections.removeAttribute("aria-busy");
      showParts(true);
      attention.draw(s, r);
      now.draw(s, words);
      for (const [chart, spec] of charts) {
        chart.update(spec(s, r));
      }
      emptyAgents.textContent = `No agent posted or read in the last ${words}.`;
      emptyThreads.textContent = `No thread had a post in the last ${words}.`;
      who.draw(s);
      threads.draw(s);
      server.draw(s, words);
      started.textContent = `Counts over time are kept in memory since the server started at ${clockTime(s.started_ms)} and reset on restart.`;
      shown = true;
    };

    const fetchStats = async (asking: AbortSignal): Promise<Answer> => {
      const r = route.range;
      const result = await getStats({ range: r }, { signal: asking });
      if (result.ok) {
        return {
          draw: () => {
            failedNotice.replaceChildren();
            staleNotice.replaceChildren();
            sections.hidden = false;
            draw(result.data, r);
            attention.show(route.problems);
            return r !== route.range;
          },
        };
      }
      return result.code === "aborted" ? null : { failure: result };
    };

    const stats = reader(signal, {
      holds: () => shown,
      load: fetchStats,
      refresh: fetchStats,
      skeleton: () => {
        failedNotice.replaceChildren();
        sections.hidden = false;
        sections.setAttribute("aria-busy", "true");
        showParts(null);
      },
      failed: (failure: Failure) => {
        sections.removeAttribute("aria-busy");
        const retry = (): void => {
          void stats.retry();
        };
        if (shown) {
          staleNotice.replaceChildren(
            notice(`The statistics could not be refreshed. ${failureWords(failure)}`, retry),
          );
          return;
        }
        sections.hidden = true;
        failedNotice.replaceChildren(
          notice(`The statistics could not be loaded. ${failureWords(failure)}`, retry),
        );
      },
    });

    const reload = (): void => {
      shown = false;
      showParts(false);
      for (const [chart] of charts) {
        chart.loading();
      }
      void stats.load();
    };

    const offer = (retentionMs: number): void => {
      const offered = offeredRanges(retentionMs);
      for (const [r, label] of choices) {
        label.hidden = !offered.includes(r);
      }
      if (!offered.includes(route.range)) {
        router.go({ ...route, range: offered.at(-1) ?? "6h" }, { replace: true });
      }
    };
    void getSettings({ signal }).then((r) => {
      if (r.ok) {
        offer(r.data.retention_ms);
      }
    });

    const timer = setInterval(() => {
      void stats.changed();
    }, refreshMs);
    signal.addEventListener(
      "abort",
      () => {
        clearInterval(timer);
      },
      { once: true },
    );
    live.watch({ resync: (outer) => stats.resync(outer) }, signal);

    void stats.load();
    return {
      update: (next) => {
        if (next.screen !== "statistics") {
          return;
        }
        const rangeChanged = next.range !== route.range;
        route = next;
        for (const [r, label] of choices) {
          const input = label.querySelector("input");
          if (input !== null) {
            input.checked = r === next.range;
          }
        }
        if (rangeChanged) {
          attention.show(null);
          reload();
        } else {
          attention.show(next.problems);
        }
      },
    };
  };
}
