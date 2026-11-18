import { afterEach, describe, expect, it, vi } from "vitest";

import type { ApiResult } from "../transport.js";
import { retryableIn, whenRetryable } from "./parts.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

const failure = (status: number, code: string, retryAfterMs: number | null = null): Failure => ({
  ok: false,
  status,
  code,
  message: null,
  retryAfterMs,
});

describe("retryableIn", () => {
  it.each([
    ["no answer", failure(0, "network"), 0],
    ["no answer in time", failure(0, "deadline"), 0],
    ["a route timeout", failure(503, "timeout", 1000), 0],
    ["a server shutting down", failure(503, "shutting_down"), 0],
    ["an unavailable account store", failure(503, "auth_unavailable"), 0],
    ["a rate limit naming its wait", failure(429, "rate_limited", 1500), 1500],
    ["a rate limit naming no wait", failure(429, "rate_limited"), 0],
  ])("offers Retry after %s", (_, f, ms) => {
    expect(retryableIn(f)).toBe(ms);
  });

  it.each([
    ["a stale revision", failure(409, "stale_revision")],
    ["a refused field", failure(400, "invalid")],
    ["a person-reserved change", failure(403, "person_reserved")],
    ["a sign-in that is not recent", failure(403, "recent_auth_required")],
    ["a lock-out", failure(409, "would_lock_out")],
    ["a full store", failure(507, "store_full")],
    ["a server failure", failure(500, "internal_error")],
    ["an answer the client could not read", failure(200, "decode")],
    ["a request the caller gave up", failure(0, "aborted")],
  ])("offers no Retry after %s", (_, f) => {
    expect(retryableIn(f)).toBeNull();
  });
});

describe("whenRetryable", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("shows Retry at once when sending again can succeed now", () => {
    const show = vi.fn();
    whenRetryable(failure(503, "timeout"), new AbortController().signal, show);
    expect(show).toHaveBeenCalledOnce();
  });

  it("shows Retry once the named wait has passed, and not before", () => {
    vi.useFakeTimers();
    const show = vi.fn();
    whenRetryable(failure(429, "rate_limited", 2000), new AbortController().signal, show);
    vi.advanceTimersByTime(1999);
    expect(show).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(show).toHaveBeenCalledOnce();
  });

  it("never shows Retry once its signal ends first", () => {
    vi.useFakeTimers();
    const show = vi.fn();
    const life = new AbortController();
    whenRetryable(failure(429, "rate_limited", 2000), life.signal, show);
    life.abort();
    vi.advanceTimersByTime(2000);
    expect(show).not.toHaveBeenCalled();
    whenRetryable(failure(503, "timeout"), life.signal, show);
    expect(show).not.toHaveBeenCalled();
  });

  it("never shows Retry when only a change can succeed", () => {
    vi.useFakeTimers();
    const show = vi.fn();
    whenRetryable(failure(409, "stale_revision"), new AbortController().signal, show);
    vi.runAllTimers();
    expect(show).not.toHaveBeenCalled();
  });
});
