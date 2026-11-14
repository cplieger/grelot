import { http, HttpResponse } from "msw";
import { setupWorker } from "msw/browser";
import { afterAll, afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { leaveTo } from "./location.js";
import { clientRequest, newIdempotencyKey, type EndpointSpec } from "./transport.js";

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

afterAll(() => {
  worker.stop();
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
    });
    worker.use(http.get("/api/thing", () => HttpResponse.json(["not_found"], { status: 404 })));
    await expect(clientRequest(spec(), asIs)).resolves.toMatchObject({
      code: "http",
      message: null,
    });
    worker.use(http.get("/api/thing", () => HttpResponse.text("Bad Gateway", { status: 502 })));
    await expect(clientRequest(spec(), asIs)).resolves.toEqual({
      ok: false,
      status: 502,
      code: "http",
      message: null,
    });
  });

  it("fails as network when no answer arrives, and as aborted when the caller gave up", async () => {
    worker.use(http.get("/api/thing", () => HttpResponse.error()));
    await expect(clientRequest(spec(), asIs)).resolves.toEqual({
      ok: false,
      status: 0,
      code: "network",
      message: null,
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

describe("newIdempotencyKey", () => {
  it("makes a fresh quoted key the server's keyed routes accept", () => {
    // internal/server/request.go parseKey: one quoted string of 16 to 64
    // letters, digits, - or _.
    const a = newIdempotencyKey();
    expect(a).toMatch(/^"[A-Za-z0-9_-]{16,64}"$/);
    expect(newIdempotencyKey()).not.toBe(a);
  });
});
