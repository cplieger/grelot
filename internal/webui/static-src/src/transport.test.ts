import { http, HttpResponse } from "msw";
import { setupWorker } from "msw/browser";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { leaveTo } from "./location.js";
import { clientRequest, newIdempotencyKey, retryAfterOf, type EndpointSpec } from "./transport.js";

vi.mock("./location.js", () => ({ leaveTo: vi.fn() }));

const worker = setupWorker();
const originalHref = location.href;

beforeAll(async () => {
  await worker.start({ quiet: true, onUnhandledFrame: "error" });
});

afterEach(() => {
  worker.resetHandlers();
  history.replaceState(null, "", originalHref);
});

const codes = ["not_found", "too_large"] as const;
type Code = (typeof codes)[number];

function spec(extra: Partial<EndpointSpec> = {}): EndpointSpec {
  return { method: "GET", path: "/api/thing", codes, ...extra };
}

const asIs = (v: unknown): unknown => v;

describe("clientRequest", () => {
  it("decodes a JSON answer", async () => {
    worker.use(http.get("/api/thing", () => HttpResponse.json({ n: 3 })));
    const decode = (v: unknown): number => (v as { n: number }).n;
    await expect(clientRequest<number, Code>(spec(), decode)).resolves.toEqual({
      ok: true,
      data: 3,
    });
  });

  it("hands the decoder the answer's status", async () => {
    worker.use(http.get("/api/thing", () => HttpResponse.json({ n: 1 }, { status: 203 })));
    const seen: number[] = [];
    await clientRequest(spec(), (v, status) => {
      seen.push(status);
      return v;
    });
    expect(seen).toEqual([203]);
  });

  it("decodes an answer with no body as undefined", async () => {
    worker.use(http.post("/api/thing", () => new HttpResponse(null, { status: 204 })));
    const seen: unknown[] = [];
    const result = await clientRequest<null, Code>(spec({ method: "POST" }), (v) => {
      seen.push(v);
      return null;
    });
    expect(result.ok).toBe(true);
    expect(seen).toEqual([undefined]);
  });

  it("sends the body as JSON with the endpoint's headers", async () => {
    let sent: { method: string; type: string | null; key: string | null; body: unknown } | null =
      null;
    worker.use(
      http.post("/api/thing", async ({ request }) => {
        sent = {
          method: request.method,
          type: request.headers.get("Content-Type"),
          key: request.headers.get("Idempotency-Key"),
          body: await request.json(),
        };
        return HttpResponse.json({}, { status: 201 });
      }),
    );
    const key = newIdempotencyKey();
    await clientRequest(
      spec({ method: "POST", body: { subject: "hi" }, headers: { "Idempotency-Key": key } }),
      asIs,
    );
    expect(sent).toEqual({
      method: "POST",
      type: "application/json",
      key,
      body: { subject: "hi" },
    });
  });

  it("fails as decode when the decoder refuses the answer or it is not JSON", async () => {
    worker.use(http.get("/api/thing", () => HttpResponse.json({ n: "x" })));
    const strict = (v: unknown): number => {
      const n = (v as { n: unknown }).n;
      if (typeof n !== "number") {
        throw new TypeError("$.n: expected number");
      }
      return n;
    };
    await expect(clientRequest(spec(), strict)).resolves.toEqual({
      ok: false,
      status: 200,
      code: "decode",
      message: null,
      retryAfterMs: null,
    });
    worker.use(http.get("/api/thing", () => HttpResponse.text("<html>")));
    await expect(clientRequest(spec(), asIs)).resolves.toMatchObject({ ok: false, code: "decode" });
  });

  it("names a refusal the endpoint lists, with the server's words", async () => {
    worker.use(
      http.get("/api/thing", () =>
        HttpResponse.json({ error: "the body is over 64 KiB", code: "too_large" }, { status: 413 }),
      ),
    );
    await expect(clientRequest<unknown, Code>(spec(), asIs)).resolves.toEqual({
      ok: false,
      status: 413,
      code: "too_large",
      message: "the body is over 64 KiB",
      retryAfterMs: null,
    });
  });

  it("reads an unlisted code, an envelope that is no object and a bare error page as http", async () => {
    worker.use(
      http.get("/api/thing", () =>
        HttpResponse.json({ error: "slow down", code: "too_many_waits" }, { status: 429 }),
      ),
    );
    await expect(clientRequest(spec(), asIs)).resolves.toEqual({
      ok: false,
      status: 429,
      code: "http",
      message: "slow down",
      retryAfterMs: null,
    });
    worker.use(http.get("/api/thing", () => HttpResponse.json(["not_found"], { status: 404 })));
    await expect(clientRequest(spec(), asIs)).resolves.toMatchObject({
      code: "http",
      message: null,
      retryAfterMs: null,
    });
    worker.use(http.get("/api/thing", () => HttpResponse.text("Bad Gateway", { status: 502 })));
    await expect(clientRequest(spec(), asIs)).resolves.toEqual({
      ok: false,
      status: 502,
      code: "http",
      message: null,
      retryAfterMs: null,
    });
  });

  it("carries the wait the answer's Retry-After names, and none for a value that is no wait", async () => {
    const now = Date.now();
    const answers: (string | null)[] = [
      "2",
      "0",
      "-5",
      "1.5",
      "soon",
      new Date(now + 60_000).toUTCString(),
      new Date(now - 60_000).toUTCString(),
      null,
    ];
    const seen: (number | null)[] = [];
    for (const wait of answers) {
      worker.use(
        http.get("/api/thing", () =>
          HttpResponse.json(
            { error: "slow down", code: "too_large", retry_after_ms: 9000 },
            { status: 429, headers: wait === null ? {} : { "Retry-After": wait } },
          ),
        ),
      );
      const result = await clientRequest(spec(), asIs);
      seen.push(result.ok ? -1 : result.retryAfterMs);
    }
    expect(seen.slice(0, 5)).toEqual([2000, 0, null, null, null]);
    expect(seen[5]).toBeGreaterThan(50_000);
    expect(seen[5]).toBeLessThanOrEqual(60_000);
    expect(seen.slice(6)).toEqual([0, null]);
  });

  it("fails as network when no answer arrives, and as aborted when the caller gave up", async () => {
    worker.use(http.get("/api/thing", () => HttpResponse.error()));
    await expect(clientRequest(spec(), asIs)).resolves.toEqual({
      ok: false,
      status: 0,
      code: "network",
      message: null,
      retryAfterMs: null,
    });
    worker.use(http.get("/api/thing", () => HttpResponse.json({})));
    const gaveUp = new AbortController();
    gaveUp.abort();
    await expect(clientRequest(spec(), asIs, { signal: gaveUp.signal })).resolves.toMatchObject({
      status: 0,
      code: "aborted",
    });
  });

  it("sends a refused session to sign-in, to come back to this address", async () => {
    history.replaceState(null, "", "/messages/7?show=direct");
    worker.use(
      http.get("/api/thing", () =>
        HttpResponse.json({ error: "sign in", code: "auth_required" }, { status: 401 }),
      ),
    );
    const result = await clientRequest(spec({ codes: ["auth_required"] }), asIs);
    expect(result).toMatchObject({ ok: false, status: 401, code: "auth_required" });
    expect(leaveTo).toHaveBeenCalledExactlyOnceWith(
      "/sign-in?next=%2Fmessages%2F7%3Fshow%3Ddirect",
    );
  });

  it("stays on a gate page when a request there is refused", async () => {
    history.replaceState(null, "", "/sign-in");
    worker.use(http.get("/api/thing", () => new HttpResponse(null, { status: 401 })));
    await clientRequest(spec(), asIs);
    expect(leaveTo).not.toHaveBeenCalled();
  });
});

describe("retryAfterOf", () => {
  // Wed, 18 Nov 2026 08:00:00 GMT.
  const now = Date.UTC(2026, 10, 18, 8, 0, 0);
  // HTML timers: setTimeout runs a longer delay at once.
  const longestTimer = 2_147_483_647;

  afterEach(() => {
    vi.useRealTimers();
  });

  function waitOf(value: string): number | null {
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(now);
    return retryAfterOf(new Headers({ "Retry-After": value }));
  }

  // RFC 9110 section 5.6.7: a recipient reads all three HTTP-date forms.
  it.each([
    ["an IMF-fixdate", "Wed, 18 Nov 2026 08:01:30 GMT", 90_000],
    [
      "an RFC 850 date, its two-digit year in this century",
      "Wednesday, 18-Nov-26 08:01:30 GMT",
      90_000,
    ],
    ["an asctime date, as GMT", "Wed Nov 18 08:01:30 2026", 90_000],
    ["an asctime date with a one-digit day", "Tue Dec  1 08:00:00 2026", 13 * 86_400_000],
    ["an RFC 850 date 49 years on", "Monday, 18-Nov-75 08:00:00 GMT", longestTimer],
    ["an RFC 850 date over 50 years on, as the past", "Thursday, 18-Nov-77 08:00:00 GMT", 0],
    ["the RFC's own IMF-fixdate, passed", "Sun, 06 Nov 1994 08:49:37 GMT", 0],
    ["the RFC's own RFC 850 date, passed", "Sunday, 06-Nov-94 08:49:37 GMT", 0],
    ["the RFC's own asctime date, passed", "Sun Nov  6 08:49:37 1994", 0],
  ])("reads %s", (_, value, ms) => {
    expect(waitOf(value)).toBe(ms);
  });

  it("holds a wait to the longest timer a browser keeps", () => {
    expect(waitOf("2147483")).toBe(2_147_483_000);
    expect(waitOf("2147484")).toBe(longestTimer);
    expect(waitOf("9".repeat(400))).toBe(longestTimer);
    expect(waitOf("Fri, 01 Jan 2100 00:00:00 GMT")).toBe(longestTimer);
  });

  it.each([
    ["a day the month lacks", "Tue, 31 Feb 2026 08:00:00 GMT"],
    ["an hour past 23", "Wed, 18 Nov 2026 24:00:00 GMT"],
    ["a zone other than GMT", "Wed, 18 Nov 2026 08:01:30 UTC"],
    ["a lowercase month", "Wed, 18 nov 2026 08:01:30 GMT"],
    ["an asctime date with a zone", "Wed Nov 18 08:01:30 2026 GMT"],
    ["an RFC 850 date with a four-digit year", "Wednesday, 18-Nov-2026 08:01:30 GMT"],
    ["an ISO 8601 instant", "2026-11-18T08:01:30Z"],
  ])("reads %s as no wait", (_, value) => {
    expect(waitOf(value)).toBeNull();
  });
});

describe("newIdempotencyKey", () => {
  it("makes a fresh quoted key the server's keyed routes accept", () => {
    // internal/server/request.go parseKey: one quoted string of 16 to 64
    // letters, digits, - or _.
    const a = newIdempotencyKey();
    expect(a).toMatch(/^"[A-Za-z0-9_-]{16,64}"$/);
    expect(newIdempotencyKey()).not.toBe(a);
  });
});
