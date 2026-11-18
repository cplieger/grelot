import { el } from "@cplieger/reactive/dom";
import { enhanceMeter } from "@cplieger/ui/meter";
import { roll } from "@cplieger/ui/roll";
import { renderSparkline } from "@cplieger/ui/sparkline";

import { byteWords, durationFigure, durationWords } from "../format.js";
import type { Sample, Stats } from "../wire/types.gen.js";

type Tone = "neutral" | "warning";

/** One .cp-figure: its label, its number rolling to each new value, and its sparkline. */
export interface Figure {
  readonly element: HTMLElement;
  /**
   * Writes value as text, or as a number on its unit's baseline, past its
   * threshold in tone with the threshold's words for a reader.
   */
  set(
    value: number,
    text: string | { readonly number: string; readonly unit: string },
    tone?: { readonly tone: Tone; readonly words: string },
  ): void;
  /** Draws values over the range; nothing for a figure without a sparkline. */
  spark(
    values: readonly (number | null)[],
    range: string,
    domain?: readonly [number, number],
  ): void;
}

interface FigureOptions {
  readonly series?: number;
  readonly format?: (n: number) => string;
  /** A button opening the list behind its count, with this dialog's id. */
  readonly opens?: string;
}

export function figure(label: string, opts: FigureOptions = {}): Figure {
  const number = el("data", { className: "cp-roll", "data-kind": "number" }) as HTMLDataElement;
  const threshold = el("span", { className: "cp-visually-hidden" });
  const unit = el("span", { "data-part": "unit" });
  const valuePart = el("span", { "data-part": "value" }, number, threshold);
  const spark = opts.series === undefined ? null : el("span", { className: "cp-sparkline" });
  const parts = [
    el("span", { "data-part": "label" }, label),
    valuePart,
    ...(spark === null ? [] : [spark]),
  ];
  const series = opts.series === undefined ? {} : { "data-series": String(opts.series) };
  const element =
    opts.opens === undefined
      ? el("div", { className: "cp-figure", ...series }, ...parts)
      : el(
          "button",
          {
            className: "cp-figure",
            type: "button",
            "aria-haspopup": "dialog",
            "aria-controls": opts.opens,
            "aria-expanded": "false",
            "data-tone": "neutral",
          },
          ...parts,
        );
  let shown: number | null = null;
  return {
    element,
    set(value, text, tone) {
      number.value = String(value);
      const words = typeof text === "string" ? text : text.number;
      if (shown === null) {
        number.textContent = words;
      } else {
        roll(number, words, { down: value < shown });
      }
      if (typeof text === "string") {
        unit.remove();
      } else {
        unit.textContent = `\u00A0${text.unit}`;
        number.after(unit);
      }
      shown = value;
      if (tone === undefined) {
        element.removeAttribute("data-tone");
      } else {
        element.setAttribute("data-tone", tone.tone);
      }
      threshold.textContent = tone?.words ?? "";
    },
    spark(values, range, domain) {
      if (spark !== null) {
        renderSparkline(spark, values, {
          range: `Last ${range}`,
          last: false,
          ...(domain === undefined ? {} : { domain }),
          ...(opts.format === undefined ? {} : { format: opts.format }),
        });
      }
    },
  };
}

const whole = (n: number): string => new Intl.NumberFormat().format(n);

/**
 * One value a minute over the range, oldest first, for a sparkline: a
 * minute with no sample, or whose store was not counted when pick reads
 * the store, is a gap.
 */
function minutes(s: Stats, pick: (m: Sample) => number, store: boolean): (number | null)[] {
  const out: (number | null)[] = Array.from(
    { length: Math.max(0, Math.round((s.end_ms - s.start_ms) / 60_000)) },
    () => null,
  );
  for (const m of s.samples) {
    const i = Math.floor((m.at_ms - s.start_ms) / 60_000);
    if (i >= 0 && i < out.length && (!store || m.counted)) {
      out[i] = pick(m);
    }
  }
  return out;
}

/** The figures' sample: the newest one whose store was counted, or the newest of all. */
function newest(s: Stats, store: boolean): Sample | undefined {
  return store ? s.samples.findLast((m) => m.counted) : s.samples.at(-1);
}

/** Oldest unread mail past this age takes the warning tone. */
const staleUnreadMs = 3_600_000;

/** A share as a percent, to one decimal: 0.002 writes "0.2%". */
const percent = (n: number): string =>
  `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(n * 100)}%`;

/** The store meter's words: use, ceiling and share, the refusal threshold, and the mark passed. */
function storeWords(inUse: number, max: number): string {
  const share = max > 0 ? inUse / max : 0;
  const passed = share > 0.9 ? ", past the 90% mark" : share >= 0.8 ? ", past the 80% mark" : "";
  return `${byteWords(inUse)} of ${byteWords(max)} (${percent(share)})${passed}. New writes stop at 90%.`;
}

/** The "Now" section: agents, mail and reservations as figures, and Store used as a meter. */
export function nowFigures(signal: AbortSignal): {
  readonly element: HTMLElement;
  draw(s: Stats, range: string): void;
} {
  const live = figure("Live agents", { series: 1 });
  const active = figure("Active", { series: 2 });
  const dormant = figure("Dormant", { series: 3 });
  const unread = figure("Unread mail", { series: 4 });
  const oldest = figure("Oldest unread", { series: 5, format: durationWords });
  const held = figure("Reservations held", { series: 6 });
  const queued = figure("Queued", { series: 7 });
  const grid = el(
    "div",
    { className: "cp-grid" },
    ...[live, active, dormant, unread, oldest, held, queued].map((f) => f.element),
  );
  const meter = el("meter", {
    id: "stats-store",
    min: "0",
    optimum: "0",
    "aria-describedby": "stats-store-words",
  }) as HTMLMeterElement;
  const words = el("small", { "data-part": "words", id: "stats-store-words" });
  const store = el(
    "div",
    { className: "cp-meter" },
    el("label", { "data-part": "label", htmlFor: meter.id }, "Store used"),
    el("div", { "data-part": "bar" }, meter),
    words,
  );
  const none = el(
    "p",
    { className: "cp-meta", hidden: true },
    "The first sample is taken a minute after the server starts.",
  );
  const element = el("div", { className: "cp-stack" }, none, grid, store);
  let enhanced = false;
  return {
    element,
    draw(s, range) {
      const m = newest(s, true);
      none.hidden = m !== undefined;
      grid.hidden = m === undefined;
      store.hidden = m === undefined;
      if (m === undefined) {
        return;
      }
      const agents: readonly [number, number] = [
        0,
        Math.max(1, ...s.samples.filter((x) => x.counted).map((x) => x.live)),
      ];
      live.set(m.live, whole(m.live));
      live.spark(
        minutes(s, (x) => x.live, true),
        range,
        agents,
      );
      active.set(m.online, whole(m.online));
      active.spark(
        minutes(s, (x) => x.online, true),
        range,
        agents,
      );
      dormant.set(m.dormant, whole(m.dormant));
      dormant.spark(
        minutes(s, (x) => x.dormant, true),
        range,
      );
      unread.set(m.unread, whole(m.unread));
      unread.spark(
        minutes(s, (x) => x.unread, true),
        range,
      );
      oldest.set(
        m.oldest_unread_ms,
        durationFigure(m.oldest_unread_ms),
        m.oldest_unread_ms > staleUnreadMs
          ? { tone: "warning", words: ", over an hour" }
          : undefined,
      );
      oldest.spark(
        minutes(s, (x) => x.oldest_unread_ms, true),
        range,
      );
      held.set(m.held, whole(m.held));
      held.spark(
        minutes(s, (x) => x.held, true),
        range,
      );
      queued.set(m.queued, whole(m.queued));
      queued.spark(
        minutes(s, (x) => x.queued, true),
        range,
      );
      meter.max = Math.max(1, m.max_bytes);
      meter.low = 0.8 * meter.max;
      meter.high = 0.9 * meter.max;
      meter.value = m.in_use_bytes;
      meter.textContent = `${byteWords(m.in_use_bytes)} of ${byteWords(m.max_bytes)}`;
      words.textContent = storeWords(m.in_use_bytes, m.max_bytes);
      if (!enhanced) {
        enhanceMeter(store, { signal });
        enhanced = true;
      }
    },
  };
}

/** The Server section's figures: requests, server errors, latency, memory and goroutines a minute. */
export function serverFigures(): {
  readonly element: HTMLElement;
  draw(s: Stats, range: string): void;
} {
  const requests = figure("Requests a minute", { series: 1 });
  const errors = figure("Server errors a minute", { series: 2 });
  const latency = figure("Latency, 95th percentile", { series: 3, format: durationWords });
  const memory = figure("Memory", { series: 4, format: byteWords });
  const goroutines = figure("Goroutines", { series: 5 });
  const element = el(
    "div",
    { className: "cp-grid" },
    ...[requests, errors, latency, memory, goroutines].map((f) => f.element),
  );
  return {
    element,
    draw(s, range) {
      const m = newest(s, false);
      element.hidden = m === undefined;
      if (m === undefined) {
        return;
      }
      requests.set(m.requests, whole(m.requests));
      requests.spark(
        minutes(s, (x) => x.requests, false),
        range,
      );
      errors.set(
        m.errors,
        whole(m.errors),
        m.errors > 0 ? { tone: "warning", words: ", above 0" } : undefined,
      );
      errors.spark(
        minutes(s, (x) => x.errors, false),
        range,
      );
      latency.set(m.p95_ms, durationWords(m.p95_ms));
      latency.spark(
        minutes(s, (x) => x.p95_ms, false),
        range,
      );
      memory.set(m.resident_bytes, byteWords(m.resident_bytes));
      memory.spark(
        minutes(s, (x) => x.resident_bytes, false),
        range,
      );
      goroutines.set(m.goroutines, whole(m.goroutines));
      goroutines.spark(
        minutes(s, (x) => x.goroutines, false),
        range,
      );
    },
  };
}
