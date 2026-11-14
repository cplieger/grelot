import { subscribe, type ReadonlySignal } from "@cplieger/reactive/signal";
import { http, HttpResponse } from "msw";
import { setupWorker } from "msw/browser";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { until } from "./__test-helpers__/until.js";
import { connectionOf, startLive, type Connection, type Live } from "./live.js";
import type { Changed } from "./wire/types.gen.js";

const worker = setupWorker();
let live: Live | null = null;
let stop = new AbortController();

beforeAll(async () => {
  await worker.start({ quiet: true, onUnhandledFrame: "error" });
});

afterEach(() => {
  live = null;
  stop.abort();
  stop = new AbortController();
  worker.resetHandlers();
});

afterAll(() => {
  worker.stop();
});

// The Go hub's wire (github.com/cplieger/go/sse/v2 docs/wire.md): a hello
// first, then each event under an <epoch>:<offset> id.
const epoch = "00000000000000a1";

function hello(resumed: boolean): string {
  const data = {
    wire: 1,
    epoch,
    floor: "0",
    head: "0",
    resumed,
    verdict: resumed ? "resumed" : "fresh",
    keepalive_ms: 15000,
    keepalive_event: "sse:keepalive",
  };
  return `event: sse:hello\ndata: ${JSON.stringify(data)}\n\n`;
}

function changed(offset: number, change: Changed): string {
  return `id: ${epoch}:${String(offset)}\nevent: changed\ndata: ${JSON.stringify(change)}\n\n`;
}

/** An event stream that sends frames and then stays open. */
function serve(...frames: string[]): void {
  worker.use(
    http.get("/api/events", () => {
      const body = new ReadableStream<Uint8Array>({
        start(controller) {
          controller.enqueue(new TextEncoder().encode(frames.join("")));
        },
      });
      return new HttpResponse(body, { headers: { "Content-Type": "text/event-stream" } });
    }),
  );
}

/** Settles once connection first reads kind. */
function reaches(connection: ReadonlySignal<Connection>, kind: Connection["kind"]): Promise<void> {
  return new Promise((done) => {
    let reached = false;
    const off = subscribe(connection, (c) => {
      if (c.kind === kind && !reached) {
        reached = true;
        done();
      }
    });
    stop.signal.addEventListener("abort", off, { once: true });
  });
}

describe("connectionOf", () => {
  it.each([
    [
      { kind: "open", generation: 1, attempt: 0, keepaliveMs: 15000, visible: true, online: true },
      { kind: "live" },
    ],
    [{ kind: "stopped", generation: 1 }, { kind: "live" }],
    [{ kind: "hidden_closed", generation: 1, attempt: 0, online: true }, { kind: "live" }],
    [
      { kind: "connecting", generation: 1, attempt: 0, visible: true, online: true },
      { kind: "live" },
    ],
    [
      { kind: "connecting", generation: 2, attempt: 1, visible: true, online: true },
      { kind: "reconnecting", since: 5 },
    ],
    [
      { kind: "backoff", generation: 2, attempt: 1, until: 9, visible: true, online: true },
      { kind: "reconnecting", since: 5 },
    ],
    [
      { kind: "offline", generation: 2, attempt: 0, visible: true },
      { kind: "offline", since: 5 },
    ],
  ] as const)("reads %o as %o", (state, want) => {
    expect(connectionOf(state, 5)).toEqual(want);
  });
});

describe("startLive", () => {
  it("refetches every open view after a fresh hello, then hands each view what changed", async () => {
    serve(hello(false), changed(1, { conversations: [7, 9], agents: true }));
    const order: string[] = [];
    let refetched = false;
    live = startLive({ signal: stop.signal, onAuthLost: () => undefined });
    live.watch(
      {
        resync: async (signal) => {
          order.push(`resync ${String(signal.aborted)}`);
          await new Promise((resolve) => setTimeout(resolve, 100));
          refetched = true;
        },
        changed: (c) => {
          order.push(
            `changed ${c.conversations.join(",")} ${String(c.agents)} after refetch ${String(refetched)}`,
          );
        },
      },
      stop.signal,
    );
    await until(() => {
      expect(order).toEqual(["resync false", "changed 7,9 true after refetch true"]);
    });
    expect(live.connection.value).toEqual({ kind: "live" });
  });

  it("applies frames at once after a hello that resumed", async () => {
    serve(hello(true), changed(4, { conversations: [3], agents: false }));
    const resync = vi.fn(() => Promise.resolve());
    const seen: Changed[] = [];
    live = startLive({ signal: stop.signal, onAuthLost: () => undefined });
    live.watch({ resync, changed: (c) => seen.push(c) }, stop.signal);
    await until(() => {
      expect(seen).toEqual([{ conversations: [3], agents: false }]);
    });
    expect(resync).not.toHaveBeenCalled();
  });

  it("stops telling a view once its signal aborts", async () => {
    serve(hello(true), changed(4, { conversations: [3], agents: false }));
    const gone = vi.fn();
    const kept: Changed[] = [];
    const leaving = new AbortController();
    live = startLive({ signal: stop.signal, onAuthLost: () => undefined });
    live.watch({ changed: gone }, leaving.signal);
    live.watch({ changed: (c) => kept.push(c) }, stop.signal);
    leaving.abort();
    await until(() => {
      expect(kept).toHaveLength(1);
    });
    expect(gone).not.toHaveBeenCalled();
  });

  it("says it is reconnecting while the server refuses the stream", async () => {
    worker.use(http.get("/api/events", () => new HttpResponse(null, { status: 503 })));
    live = startLive({ signal: stop.signal, onAuthLost: () => undefined });
    await reaches(live.connection, "reconnecting");
    expect(live.connection.value).toEqual({ kind: "reconnecting", since: null });
  });

  it("says since when the stream dropped after it was open", async () => {
    let first = true;
    let drop = (): void => undefined;
    worker.use(
      http.get("/api/events", () => {
        if (!first) {
          return new HttpResponse(null, { status: 503 });
        }
        first = false;
        const body = new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(new TextEncoder().encode(hello(false)));
            drop = () => {
              controller.close();
            };
          },
        });
        return new HttpResponse(body, { headers: { "Content-Type": "text/event-stream" } });
      }),
    );
    const opened = Promise.withResolvers<undefined>();
    live = startLive({ signal: stop.signal, onAuthLost: () => undefined, now: () => 1234 });
    live.watch(
      {
        resync: () => {
          opened.resolve(undefined);
          return Promise.resolve();
        },
      },
      stop.signal,
    );
    await opened.promise;
    drop();
    await reaches(live.connection, "reconnecting");
    expect(live.connection.value).toEqual({ kind: "reconnecting", since: 1234 });
  });

  it("reports a refused session once, without retrying", async () => {
    let asked = 0;
    worker.use(
      http.get("/api/events", () => {
        asked++;
        return HttpResponse.json({ error: "sign in", code: "auth_required" }, { status: 401 });
      }),
    );
    const onAuthLost = vi.fn();
    live = startLive({ signal: stop.signal, onAuthLost });
    await until(() => {
      expect(onAuthLost).toHaveBeenCalledOnce();
    });
    expect(asked).toBe(1);
  });
});
