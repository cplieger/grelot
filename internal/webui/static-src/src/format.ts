/**
 * A moment as a reader names it in a sentence: its clock time when it is
 * today in now's day, else its date and time, in the locale's words.
 */
export function clockTime(at: number, now: number = Date.now(), locale?: string): string {
  const moment = new Date(at);
  const today = new Date(now);
  const sameDay =
    moment.getFullYear() === today.getFullYear() &&
    moment.getMonth() === today.getMonth() &&
    moment.getDate() === today.getDate();
  const format = sameDay
    ? new Intl.DateTimeFormat(locale, { timeStyle: "short" })
    : new Intl.DateTimeFormat(locale, { dateStyle: "medium", timeStyle: "short" });
  return format.format(moment);
}

/**
 * How long history is kept, as the words after "in the last": "72 hours",
 * "90 minutes", or the unit alone for one ("hour"). The server keeps it in
 * whole minutes.
 */
export function retentionWords(ms: number, locale?: string): string {
  const hours = ms / 3_600_000;
  const [n, unit] = Number.isInteger(hours) ? [hours, "hour"] : [Math.round(ms / 60_000), "minute"];
  return n === 1 ? unit : `${new Intl.NumberFormat(locale).format(n)} ${unit}s`;
}

/** The local calendar day of a moment, as a <time> element's YYYY-MM-DD. */
export function dayStamp(at: number): string {
  const d = new Date(at);
  const pad = (n: number): string => String(n).padStart(2, "0");
  return `${String(d.getFullYear())}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/**
 * A feed's day separator: the weekday, day and month, with the year when
 * it is not now's year.
 */
export function dayWords(at: number, now: number = Date.now(), locale?: string): string {
  const day = new Date(at);
  const sameYear = day.getFullYear() === new Date(now).getFullYear();
  return new Intl.DateTimeFormat(locale, {
    weekday: "long",
    day: "numeric",
    month: "long",
    ...(sameYear ? {} : { year: "numeric" }),
  }).format(day);
}

const durationUnits = [
  ["days", 86_400_000],
  ["hours", 3_600_000],
  ["minutes", 60_000],
  ["seconds", 1_000],
  ["milliseconds", 1],
] as const;

const figureUnits = [
  ["day", 86_400_000],
  ["hour", 3_600_000],
  ["minute", 60_000],
  ["second", 1_000],
] as const;

/**
 * A duration as a figure's number and unit ("9.8" "min"): the largest unit
 * it reaches, one decimal under 10, seconds at the least. A .cp-figure holds
 * one number on its unit's baseline, which the chart's two-unit words are not.
 */
export function durationFigure(
  ms: number,
  locale?: string,
): { readonly number: string; readonly unit: string } {
  const found = figureUnits.findIndex(([, size]) => ms >= size);
  let [unit, size] = figureUnits[found] ?? figureUnits[3];
  const rounded = (n: number): number => Number(n.toFixed(n < 10 ? 1 : 0));
  // Rounding can carry into the next unit up, such as 59.97 min to 1 hr.
  const up = figureUnits[found - 1];
  if (up !== undefined && rounded(ms / size) * size >= up[1]) {
    [unit, size] = up;
  }
  const n = ms / size;
  const parts = new Intl.NumberFormat(locale, {
    style: "unit",
    unit,
    unitDisplay: "short",
    maximumFractionDigits: n < 10 ? 1 : 0,
  }).formatToParts(n);
  return {
    number: parts
      .filter((p) => p.type !== "unit" && p.type !== "literal")
      .map((p) => p.value)
      .join(""),
    unit: parts
      .filter((p) => p.type === "unit")
      .map((p) => p.value)
      .join(""),
  };
}

/**
 * A duration in the chart's words ("1 min, 27 sec", "18 min", "400 ms"):
 * under 10 of its largest unit it keeps the next unit too, from 10 up the
 * largest alone, as @cplieger/ui/chart's "duration" format writes it.
 */
export function durationWords(ms: number, locale?: string): string {
  const found = durationUnits.findIndex(([, size]) => ms >= size);
  const at = found === -1 ? durationUnits.length - 1 : found;
  const [big, bigSize] = durationUnits[at] ?? durationUnits[4];
  const next = durationUnits[at + 1];
  const up = durationUnits[at - 1];
  let record: Partial<Record<(typeof durationUnits)[number][0], number>>;
  if (next === undefined || ms / bigSize >= 10) {
    const count = Math.round(ms / bigSize);
    // Rounding can carry into the next unit up, such as 59.6 min to 1 hr.
    record = up !== undefined && count * bigSize >= up[1] ? { [up[0]]: 1 } : { [big]: count };
  } else {
    const total = Math.round(ms / next[1]);
    const per = bigSize / next[1];
    const whole = Math.floor(total / per);
    const rest = total - whole * per;
    record = rest === 0 ? { [big]: whole } : { [big]: whole, [next[0]]: rest };
  }
  const zero = Object.values(record).every((v) => v === 0);
  return new Intl.DurationFormat(locale, {
    style: "short",
    ...(zero ? { millisecondsDisplay: "always" as const } : {}),
  })
    .format(zero ? { milliseconds: 0 } : record)
    .replace(/(\p{Nd}) (?=\p{L})/gu, "$1\u00A0");
}

const byteUnits = ["B", "KiB", "MiB", "GiB", "TiB"] as const;

/** A byte count in binary units, one decimal past the first: "512 MiB", "3.1 KiB". */
export function byteWords(n: number, locale?: string): string {
  let unit = 0;
  let v = n;
  while (v >= 1024 && unit < byteUnits.length - 1) {
    v /= 1024;
    unit++;
  }
  const digits = new Intl.NumberFormat(locale, { maximumFractionDigits: unit === 0 ? 0 : 1 });
  return `${digits.format(v)}\u00A0${byteUnits[unit] ?? "B"}`;
}

const ordinalRules = new Intl.PluralRules("en", { type: "ordinal" });
const ordinalSuffix = new Map<Intl.LDMLPluralRule, string>([
  ["one", "st"],
  ["two", "nd"],
  ["few", "rd"],
]);

/** n as an English ordinal: 1st, 2nd, 3rd, 4th, 11th, 21st. */
export function ordinal(n: number): string {
  return `${String(n)}${ordinalSuffix.get(ordinalRules.select(n)) ?? "th"}`;
}
