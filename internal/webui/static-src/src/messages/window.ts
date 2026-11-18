import type { ApiResult } from "../transport.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

/** Loaded pages of a paged read, newest first, and the cursor of the page after them. */
export interface Window<T, C> {
  readonly items: readonly T[];
  readonly next: C | undefined;
}

interface Rewalk<T, C> {
  /** What the view holds. */
  readonly held: Window<T, C>;
  /** The newest page, fetched again. */
  readonly fresh: Window<T, C>;
  /** Fetches the page after cursor. */
  readonly more: (cursor: C) => Promise<ApiResult<Window<T, C>, string>>;
  /** Negative when a sorts before b, the server's order. */
  readonly order: (a: T, b: T) => number;
  readonly id: (item: T) => number;
  /** The cursor whose page starts right after item. */
  readonly cursorOf: (item: T) => C;
}

/**
 * Reads the held window again, from the newest page down to the held
 * oldest's place, so an item that expired leaves and one that changed is
 * redrawn wherever it sits. Short of the oldest, the answer covers the
 * same reach as held: items read past it are dropped and next resumes
 * after the kept oldest. A failed page answers its failure alone, so no
 * partial window reaches the view; null when aborted.
 */
export async function rewalk<T, C>(
  w: Rewalk<T, C>,
): Promise<
  { readonly window: Window<T, C>; readonly failure: null } | { readonly failure: Failure } | null
> {
  const bound = w.held.items.at(-1);
  if (bound === undefined) {
    return { window: w.fresh, failure: null };
  }
  const items = [...w.fresh.items];
  const seen = new Set(items.map(w.id));
  let next = w.fresh.next;
  for (;;) {
    const last = items.at(-1);
    if (next === undefined || (last !== undefined && w.order(last, bound) >= 0)) {
      break;
    }
    const page = await w.more(next);
    if (!page.ok) {
      if (page.code === "aborted") {
        return null;
      }
      return { failure: page };
    }
    for (const t of page.data.items) {
      if (!seen.has(w.id(t))) {
        seen.add(w.id(t));
        items.push(t);
      }
    }
    next = page.data.next;
  }
  if (next === undefined) {
    return { window: { items, next }, failure: null };
  }
  const kept = items.filter((t) => w.order(t, bound) <= 0);
  const keptLast = kept.at(-1);
  return keptLast === undefined
    ? { window: { items, next }, failure: null }
    : { window: { items: kept, next: w.cursorOf(keptLast) }, failure: null };
}
