import { describe, expect, it } from "vitest";

import {
  byteWords,
  clockTime,
  dayStamp,
  dayWords,
  durationFigure,
  durationWords,
  ordinal,
  retentionWords,
} from "./format.js";

describe("clockTime", () => {
  const now = new Date(2026, 10, 13, 9, 30).getTime();

  it("gives the clock time alone for a moment earlier today", () => {
    expect(clockTime(new Date(2026, 10, 13, 0, 1).getTime(), now, "en-GB")).toBe("00:01");
  });

  it("adds the date for a moment on another day, however close", () => {
    expect(clockTime(new Date(2026, 10, 12, 23, 59).getTime(), now, "en-GB")).toBe(
      "12 Nov 2026, 23:59",
    );
    expect(clockTime(new Date(2025, 10, 13, 9, 30).getTime(), now, "en-GB")).toBe(
      "13 Nov 2025, 09:30",
    );
  });

  it("speaks the reader's locale", () => {
    expect(clockTime(new Date(2026, 10, 13, 14, 2).getTime(), now, "en-US")).toMatch(/^2:02\sPM$/u);
  });
});

describe("retentionWords", () => {
  const minute = 60_000;

  it("counts whole hours in hours, the server's default 72 among them", () => {
    expect(retentionWords(72 * 60 * minute, "en-GB")).toBe("72 hours");
    expect(retentionWords(2160 * 60 * minute, "en-GB")).toBe("2,160 hours");
  });

  it("counts a part hour in minutes", () => {
    expect(retentionWords(90 * minute, "en-GB")).toBe("90 minutes");
  });

  it("names a single unit alone, as in the last hour", () => {
    expect(retentionWords(60 * minute, "en-GB")).toBe("hour");
  });
});

describe("dayStamp", () => {
  it("is the local calendar day, whatever the hour", () => {
    expect(dayStamp(new Date(2026, 0, 5, 0, 0).getTime())).toBe("2026-01-05");
    expect(dayStamp(new Date(2026, 10, 13, 23, 59).getTime())).toBe("2026-11-13");
  });
});

describe("dayWords", () => {
  const now = new Date(2026, 10, 13, 9, 30).getTime();

  it("names the weekday, day and month within the year", () => {
    expect(dayWords(new Date(2026, 10, 11, 18, 0).getTime(), now, "en-GB")).toMatch(
      /^Wednesday,? 11 November$/u,
    );
  });

  it("adds the year for a day in another year", () => {
    expect(dayWords(new Date(2025, 11, 31, 18, 0).getTime(), now, "en-GB")).toMatch(
      /^Wednesday,? 31 December 2025$/u,
    );
  });
});

describe("ordinal", () => {
  it("follows English: st, nd and rd but in the teens, th otherwise", () => {
    expect([1, 2, 3, 4, 11, 12, 13, 21, 22, 23, 101, 111].map(ordinal)).toEqual([
      "1st",
      "2nd",
      "3rd",
      "4th",
      "11th",
      "12th",
      "13th",
      "21st",
      "22nd",
      "23rd",
      "101st",
      "111th",
    ]);
  });
});

describe("durationWords", () => {
  // The chart's "duration" format (@cplieger/ui docs/data/chart.md "Formats"):
  // under 10 of its largest unit a value keeps the next unit, from 10 up the largest alone.
  it.each([
    [0, "0\u00A0ms"],
    [400, "400\u00A0ms"],
    [87_000, "1\u00A0min, 27\u00A0sec"],
    [18 * 60_000 + 20_000, "18\u00A0min"],
    [9 * 3_600_000 + 30 * 60_000, "9\u00A0hr, 30\u00A0min"],
    [3_576_000, "1\u00A0hr"],
    [3 * 86_400_000, "3\u00A0days"],
  ])("writes %d ms as %s", (ms, want) => {
    expect(durationWords(ms, "en")).toBe(want);
  });
});

describe("durationFigure", () => {
  // A .cp-figure's value is one number on its unit's baseline (@cplieger/ui
  // design-components.md ".cp-figure"), in Intl's short unit words.
  it.each([
    [0, "0", "sec"],
    [45_000, "45", "sec"],
    [5 * 60_000, "5", "min"],
    [9 * 60_000 + 45_000, "9.8", "min"],
    [18 * 60_000 + 20_000, "18", "min"],
    [59 * 60_000 + 58_000, "1", "hr"],
    [3_660_000, "1", "hr"],
    [36 * 3_600_000, "1.5", "days"],
    [12 * 86_400_000, "12", "days"],
  ])("writes %d ms as %s %s", (ms, number, unit) => {
    expect(durationFigure(ms, "en")).toEqual({ number, unit });
  });
});

describe("byteWords", () => {
  it.each([
    [100, "100\u00A0B"],
    [1023, "1,023\u00A0B"],
    [1024, "1\u00A0KiB"],
    [3174, "3.1\u00A0KiB"],
    [512 * 1024 * 1024, "512\u00A0MiB"],
    [1536 * 1024 * 1024, "1.5\u00A0GiB"],
  ])("writes %d bytes as %s", (n, want) => {
    expect(byteWords(n, "en")).toBe(want);
  });
});
