import { describe, expect, it } from "vitest";

import type { ApiResult } from "../transport.js";
import { rewalk, type Window } from "./window.js";

/** Ids newest first; a page holds two, and its cursor is the before of the page after it. */
type Pages = Readonly<Record<number, ApiResult<Window<number, number>, string>>>;

function walk(held: Window<number, number>, fresh: Window<number, number>, pages: Pages) {
  const asked: number[] = [];
  const result = rewalk<number, number>({
    held,
    fresh,
    more: (cursor) => {
      asked.push(cursor);
      const page = pages[cursor];
      if (page === undefined) {
        throw new Error(`no page before ${String(cursor)}`);
      }
      return Promise.resolve(page);
    },
    order: (a, b) => b - a,
    id: (n) => n,
    cursorOf: (n) => n,
  });
  return { asked, result };
}

const ok = (items: number[], next?: number): ApiResult<Window<number, number>, string> => ({
  ok: true,
  data: { items, next },
});
const failed = (code: string): ApiResult<Window<number, number>, string> => ({
  ok: false,
  status: code === "aborted" ? 0 : 503,
  code,
  message: code === "aborted" ? null : "The store is busy.",
});

describe("rewalk", () => {
  it("answers the fresh page alone when nothing is held", async () => {
    const { asked, result } = walk({ items: [], next: undefined }, { items: [9, 8], next: 8 }, {});
    expect(await result).toEqual({ window: { items: [9, 8], next: 8 }, failure: null });
    expect(asked).toEqual([]);
  });

  it("reads down to the held oldest and resumes after the kept oldest", async () => {
    const { asked, result } = walk(
      { items: [9, 8, 6, 5], next: 5 },
      { items: [10, 9], next: 9 },
      { 9: ok([8, 5], 5), 5: ok([4, 3], 3) },
    );
    expect(await result).toEqual({ window: { items: [10, 9, 8, 5], next: 5 }, failure: null });
    expect(asked).toEqual([9]);
  });

  it("drops what it read past the held oldest, which Show older reads again", async () => {
    const { result } = walk(
      { items: [9, 8, 6, 5], next: 5 },
      { items: [9, 8], next: 8 },
      { 8: ok([6, 4], 4) },
    );
    expect(await result).toEqual({ window: { items: [9, 8, 6], next: 6 }, failure: null });
  });

  it("ends with no cursor at the oldest, once the held oldest expired", async () => {
    const { result } = walk(
      { items: [9, 8, 6], next: undefined },
      { items: [9, 8], next: 8 },
      { 8: ok([]) },
    );
    expect(await result).toEqual({ window: { items: [9, 8], next: undefined }, failure: null });
  });

  it("answers the fresh page with its cursor once every held item expired", async () => {
    const { asked, result } = walk(
      { items: [9, 8, 6, 5], next: 5 },
      { items: [4, 3], next: 3 },
      {},
    );
    expect(await result).toEqual({ window: { items: [4, 3], next: 3 }, failure: null });
    expect(asked).toEqual([]);
  });

  it("answers only the failure when a page fails", async () => {
    const { result } = walk(
      { items: [9, 8, 6, 5], next: 5 },
      { items: [10, 9], next: 9 },
      { 9: failed("unavailable") },
    );
    const walked = await result;
    expect(walked).not.toHaveProperty("window");
    expect(walked?.failure?.message).toBe("The store is busy.");
  });

  it("answers only the failure when the page after a fresh page wholly newer than held fails", async () => {
    const { result } = walk(
      { items: [4, 3, 2, 1], next: undefined },
      { items: [8, 7], next: 7 },
      { 7: failed("unavailable") },
    );
    const walked = await result;
    expect(walked).not.toHaveProperty("window");
    expect(walked?.failure?.message).toBe("The store is busy.");
  });

  it("answers null when a page's request was aborted", async () => {
    const { result } = walk(
      { items: [9, 8, 6, 5], next: 5 },
      { items: [10, 9], next: 9 },
      { 9: failed("aborted") },
    );
    expect(await result).toBeNull();
  });
});
