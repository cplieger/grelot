import { signal, type ReadonlySignal } from "@cplieger/reactive/signal";
import { createStream, createVersionMap, type StreamState, type TimingConfig } from "@cplieger/sse";

import { eventsEndpoint } from "./wire/client.gen.js";
import { decodeEventFrame } from "./wire/events.gen.js";
import type { Changed } from "./wire/types.gen.js";

/**
 * What the header says about the live stream. since is when it last
 * stopped being open, so what the page shows was loaded then; null when
 * it never opened.
 */
export type Connection =
  | { readonly kind: "live" }
  | { readonly kind: "reconnecting"; readonly since: number | null }
  | { readonly kind: "offline"; readonly since: number | null };

/** What an open view does when the server's data moves. */
interface Watcher {
  /** A commit wrote what change names. */
  readonly changed?: (change: Changed) => void;
  /**
   * The stream could not replay what was missed, or the page woke: refetch
   * everything the view shows, passing signal to every request. Frames that
   * arrive meanwhile wait until every watcher's promise settles.
   */
  readonly resync?: (signal: AbortSignal) => Promise<void>;
}

export interface Live {
  readonly connection: ReadonlySignal<Connection>;
  /** Registers watcher until lifetime aborts. */
  watch(watcher: Watcher, lifetime: AbortSignal): void;
  /** Connects again at once, as the header status's Try now asks. */
  reconnect(): void;
}

interface LiveOptions {
  /** Closes the stream when it aborts. */
  readonly signal: AbortSignal;
  /** The session ended: a 401 on the stream. */
  readonly onAuthLost: () => void;
  readonly timing?: Partial<TimingConfig>;
  readonly now?: () => number;
}

/**
 * The header's reading of a stream state. A first connect, and a quick
 * reconnect after a hidden tab returns (attempt 0), read as live, so the
 * status never flashes; a backoff or a retry reads as reconnecting.
 */
export function connectionOf(state: StreamState, lostAt: number | null): Connection {
  switch (state.kind) {
    case "open":
    case "stopped":
    case "hidden_closed":
      return { kind: "live" };
    case "connecting":
      return state.attempt === 0 ? { kind: "live" } : { kind: "reconnecting", since: lostAt };
    case "backoff":
      return { kind: "reconnecting", since: lostAt };
    case "offline":
      return { kind: "offline", since: lostAt };
  }
}

function sameConnection(a: Connection, b: Connection): boolean {
  return a.kind === b.kind && (a.kind === "live" || b.kind === "live" || a.since === b.since);
}

/** Opens the web API's event stream (/api/events) and keeps it open. */
export function startLive(opts: LiveOptions): Live {
  const now = opts.now ?? Date.now;
  const watchers = new Set<Watcher>();
  const connection = signal<Connection>({ kind: "live" }, { equals: sameConnection });
  let lostAt: number | null = null;

  const stream = createStream({
    url: eventsEndpoint.request().path,
    versions: createVersionMap(),
    ...(opts.timing === undefined ? {} : { timing: opts.timing }),
    // A frame that does not decode throws, which the stream answers with a
    // resync, the right reading of a frame this client cannot apply.
    onFrame: (frame) => {
      const decoded = decodeEventFrame(frame.type, JSON.parse(frame.data));
      for (const w of watchers) {
        w.changed?.(decoded.data);
      }
    },
    revalidate: async (ctx) => {
      await Promise.all(
        [...watchers].flatMap((w) => (w.resync === undefined ? [] : [w.resync(ctx.signal)])),
      );
    },
    onLifecycle: (ev) => {
      if (ev.kind === "auth_lost") {
        opts.onAuthLost();
        return;
      }
      if (ev.kind !== "state") {
        return;
      }
      if (ev.from === "open") {
        lostAt = now();
      }
      connection.value = connectionOf(stream.state(), lostAt);
    },
  });
  stream.start();
  opts.signal.addEventListener(
    "abort",
    () => {
      stream.stop();
    },
    { once: true },
  );

  return {
    connection,
    watch(watcher, lifetime) {
      watchers.add(watcher);
      lifetime.addEventListener("abort", () => watchers.delete(watcher), { once: true });
    },
    reconnect: () => {
      stream.reconnect();
    },
  };
}
