import { describe, expect, it } from "vitest";

import { reader, type Answer } from "./reader.js";

type Kind = "load" | "refresh";

interface Call {
  readonly kind: Kind;
  readonly asking: AbortSignal;
  /** Answers the read: drawn under name, or a failure. */
  readonly answer: (to: { readonly draw: string; readonly again?: boolean } | "fail") => void;
}

const busy = {
  ok: false,
  code: "timeout",
  message: "The store is busy.",
  status: 503,
} as const;

/** A view whose every read waits to be answered, recording what reached its screen. */
function fakeView() {
  const calls: Call[] = [];
  const screen: string[] = [];
  let holds = false;
  const read =
    (kind: Kind) =>
    (asking: AbortSignal): Promise<Answer> => {
      const done = Promise.withResolvers<Answer>();
      asking.addEventListener("abort", () => {
        done.resolve(null);
      });
      calls.push({
        kind,
        asking,
        answer: (to) => {
          if (to === "fail") {
            done.resolve({ failure: busy });
            return;
          }
          done.resolve({
            draw: () => {
              holds = true;
              screen.push(to.draw);
              return to.again ?? false;
            },
          });
        },
      });
      return done.promise;
    };
  const page = new AbortController();
  const r = reader(page.signal, {
    holds: () => holds,
    load: read("load"),
    refresh: read("refresh"),
    skeleton: () => {
      screen.push("skeleton");
    },
    failed: () => {
      screen.push("failed");
    },
  });
  return { r, calls, screen, page };
}

const tick = (): Promise<void> => new Promise((resolve) => setTimeout(resolve));

type View = ReturnType<typeof fakeView>;

const call = (v: View, i: number): Call => {
  const c = v.calls[i];
  if (c === undefined) {
    throw new Error(`no read ${String(i)}`);
  }
  return c;
};

/** Each case is one rule of reader's contract, run against a fresh view. */
const rules: readonly { readonly rule: string; readonly run: (v: View) => Promise<void> }[] = [
  {
    rule: "latest wins: a load aborts the read in flight, whose answer never commits",
    run: async (v) => {
      void v.r.load();
      const done = v.r.load();
      expect(call(v, 0).asking.aborted).toBe(true);
      call(v, 1).answer({ draw: "second" });
      call(v, 0).answer({ draw: "first" });
      await done;
      expect(v.screen).toEqual(["second"]);
    },
  },
  {
    rule: "latest wins: a resync aborts the running refresh and reads under its signal",
    run: async (v) => {
      void v.r.load();
      call(v, 0).answer({ draw: "loaded" });
      await tick();
      void v.r.changed();
      const outer = new AbortController();
      const done = v.r.resync(outer.signal);
      expect(call(v, 1).asking.aborted).toBe(true);
      call(v, 1).answer({ draw: "stale" });
      outer.abort();
      expect(call(v, 2).asking.aborted).toBe(true);
      await done;
      expect(v.screen).toEqual(["loaded"]);
    },
  },
  {
    rule: "a failure keeps the screen and shows the failure",
    run: async (v) => {
      void v.r.load();
      call(v, 0).answer({ draw: "loaded" });
      await tick();
      const done = v.r.changed();
      call(v, 1).answer("fail");
      await done;
      expect(v.screen).toEqual(["loaded", "failed"]);
    },
  },
  {
    rule: "Retry joins the running read: one request however often it is pressed",
    run: async (v) => {
      void v.r.load();
      call(v, 0).answer("fail");
      await tick();
      void v.r.retry();
      const done = v.r.retry();
      expect(v.calls.length).toBe(2);
      call(v, 1).answer({ draw: "loaded" });
      await done;
      expect(v.calls.length).toBe(2);
      expect(v.screen).toEqual(["failed", "loaded"]);
    },
  },
  {
    rule: "changes during a read coalesce into one more read",
    run: async (v) => {
      void v.r.load();
      call(v, 0).answer({ draw: "loaded" });
      await tick();
      const done = v.r.changed();
      void v.r.changed();
      void v.r.changed();
      expect(v.calls.length).toBe(2);
      call(v, 1).answer({ draw: "one" });
      await tick();
      expect(call(v, 1).asking.aborted).toBe(false);
      expect(call(v, 2).kind).toBe("refresh");
      call(v, 2).answer({ draw: "two" });
      await done;
      expect(v.calls.length).toBe(3);
      expect(v.screen).toEqual(["loaded", "one", "two"]);
    },
  },
  {
    rule: "an answer that no longer fits what the view holds reads again",
    run: async (v) => {
      void v.r.load();
      call(v, 0).answer({ draw: "loaded" });
      await tick();
      const done = v.r.changed();
      call(v, 1).answer({ draw: "moved", again: true });
      await tick();
      call(v, 2).answer({ draw: "current" });
      await done;
      expect(v.calls.length).toBe(3);
    },
  },
  {
    rule: "a change after a failed first load loads again",
    run: async (v) => {
      void v.r.load();
      call(v, 0).answer("fail");
      await tick();
      const done = v.r.changed();
      expect(call(v, 1).kind).toBe("load");
      call(v, 1).answer({ draw: "loaded" });
      await done;
      expect(v.screen).toEqual(["failed", "loaded"]);
    },
  },
  {
    rule: "a resync after a failed first load loads again",
    run: async (v) => {
      void v.r.load();
      call(v, 0).answer("fail");
      await tick();
      const done = v.r.resync(new AbortController().signal);
      expect(call(v, 1).kind).toBe("load");
      call(v, 1).answer({ draw: "loaded" });
      await done;
      expect(v.screen).toEqual(["failed", "loaded"]);
    },
  },
  {
    rule: "a change during a first load that fails loads again once the failure is shown",
    run: async (v) => {
      const done = v.r.load();
      void v.r.changed();
      call(v, 0).answer("fail");
      await tick();
      expect(v.screen).toEqual(["failed"]);
      expect(call(v, 1).kind).toBe("load");
      call(v, 1).answer({ draw: "loaded" });
      await done;
      expect(v.screen).toEqual(["failed", "loaded"]);
    },
  },
  {
    rule: "a resync under an aborted signal leaves later changes reading",
    run: async (v) => {
      void v.r.load();
      call(v, 0).answer({ draw: "loaded" });
      await tick();
      const gone = new AbortController();
      gone.abort();
      await v.r.resync(gone.signal);
      const done = v.r.changed();
      expect(v.calls.length).toBe(2);
      call(v, 1).answer({ draw: "changed" });
      await done;
      expect(v.screen).toEqual(["loaded", "changed"]);
    },
  },
  {
    rule: "nothing reads before the first load",
    run: async (v) => {
      await v.r.changed();
      await v.r.retry();
      await v.r.resync(new AbortController().signal);
      expect(v.calls.length).toBe(0);
    },
  },
  {
    rule: "teardown aborts the read in flight, whose answer never commits",
    run: async (v) => {
      const done = v.r.load();
      v.page.abort();
      expect(call(v, 0).asking.aborted).toBe(true);
      call(v, 0).answer({ draw: "late" });
      await done;
      expect(v.screen).toEqual([]);
    },
  },
];

describe("reader", () => {
  it.each(rules)("$rule", async ({ run }) => {
    await run(fakeView());
  });

  it("never draws a slow load's answer held behind its skeleton once a newer load replaced it", async () => {
    const v = fakeView();
    void v.r.load();
    await new Promise((resolve) => setTimeout(resolve, 250));
    call(v, 0).answer({ draw: "first" });
    await tick();
    const done = v.r.load();
    call(v, 1).answer({ draw: "second" });
    await done;
    await new Promise((resolve) => setTimeout(resolve, 450));
    expect(v.screen).toEqual(["skeleton", "second"]);
  });

  it("shows the skeleton while a slow load waits, and never for a refresh", async () => {
    const v = fakeView();
    void v.r.load();
    await new Promise((resolve) => setTimeout(resolve, 250));
    expect(v.screen).toEqual(["skeleton"]);
    call(v, 0).answer({ draw: "loaded" });
    await new Promise((resolve) => setTimeout(resolve, 450));
    const done = v.r.changed();
    await new Promise((resolve) => setTimeout(resolve, 250));
    call(v, 1).answer({ draw: "refreshed" });
    await done;
    expect(v.screen).toEqual(["skeleton", "loaded", "refreshed"]);
  });
});
