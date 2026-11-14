import { skeletonTiming } from "@cplieger/ui/skeleton";

import type { ApiResult } from "../transport.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

/** What one read fetched; null when it was aborted. */
export type Answer =
  /** Puts the answer on screen; true when it no longer fits what the view holds, to read again. */
  { readonly draw: () => boolean } | { readonly failure: Failure } | null;

/** One view's reads, which reader sequences. */
export interface ReadSource {
  /** The view shows an answer a refresh can read again; otherwise a read loads. */
  readonly holds: () => boolean;
  /** Fetches the view from nothing. */
  readonly load: (asking: AbortSignal) => Promise<Answer>;
  /** Fetches again what the view holds. */
  readonly refresh: (asking: AbortSignal) => Promise<Answer>;
  /** Shown while a slow load waits. */
  readonly skeleton: () => void;
  /** Shows failure: as the refresh notice over what the view holds, otherwise as its failed state. */
  readonly failed: (failure: Failure) => void;
}

/** A view's reads: only the newest request's answer ever reaches the screen. */
export interface Reader {
  /** Aborts any read and loads; until the first load, changes, resyncs and retries do nothing. */
  load(): Promise<void>;
  /** Something the view shows changed: reads it, or once more after the running read. */
  changed(): Promise<void>;
  /** Aborts any read and reads again under outer, a resync's signal. */
  resync(outer: AbortSignal): Promise<void>;
  /** Joins the running read, or reads: one request however often it is pressed. */
  retry(): Promise<void>;
}

/**
 * Sequences one view's reads. A load or a resync aborts the read in flight,
 * and an answer commits only while its read is current; changes arriving
 * during a read coalesce into one more, because aborting on each would
 * starve every read under a busy stream.
 */
export function reader(signal: AbortSignal, source: ReadSource): Reader {
  let started = false;
  let stop: AbortController | null = null;
  let running: Promise<void> | null = null;
  let dirty = false;

  const once = async (loading: boolean, asking: AbortSignal): Promise<boolean> => {
    const timing = loading
      ? skeletonTiming(source.skeleton, { showDelayMs: 200, minVisibleMs: 400, signal: asking })
      : null;
    const answer = await (loading ? source.load(asking) : source.refresh(asking));
    if (answer === null || asking.aborted) {
      timing?.cancel();
      return false;
    }
    const commit = (): boolean => {
      if (asking.aborted) {
        return false;
      }
      if ("failure" in answer) {
        source.failed(answer.failure);
        return false;
      }
      return answer.draw();
    };
    if (timing === null) {
      return commit();
    }
    const done = Promise.withResolvers<boolean>();
    timing.commit(() => {
      try {
        done.resolve(commit());
      } catch (err: unknown) {
        done.reject(err);
      }
    });
    return done.promise;
  };

  const start = (first: "load" | "auto", outer?: AbortSignal): Promise<void> => {
    stop?.abort();
    const mine = new AbortController();
    stop = mine;
    dirty = false;
    const asking = AbortSignal.any([signal, mine.signal, ...(outer === undefined ? [] : [outer])]);
    const current = (async () => {
      try {
        let loading = first === "load";
        let again = true;
        while (again && !asking.aborted) {
          dirty = false;
          again = (await once(loading || !source.holds(), asking)) || dirty;
          loading = false;
        }
      } finally {
        if (stop === mine) {
          stop = null;
          running = null;
        }
      }
    })();
    // A read under an already aborted signal settles synchronously, and its finally has run.
    if (stop === mine) {
      running = current;
    }
    return current;
  };

  return {
    load() {
      started = true;
      return start("load");
    },
    changed() {
      if (!started) {
        return Promise.resolve();
      }
      if (running !== null) {
        dirty = true;
        return running;
      }
      return start("auto");
    },
    resync(outer) {
      return started ? start("auto", outer) : Promise.resolve();
    },
    retry() {
      if (!started) {
        return Promise.resolve();
      }
      return running ?? start("auto");
    },
  };
}
