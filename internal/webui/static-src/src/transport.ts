import { leaveTo } from "./location.js";
import { signInHref } from "./router.js";

type HttpMethod = "GET" | "POST" | "PUT" | "PATCH" | "DELETE";

/** One request as the generated client describes it (wiregen docs/http-client.md). */
export interface EndpointSpec {
  readonly method: HttpMethod;
  readonly path: string;
  readonly raw?: true;
  readonly body?: unknown;
  readonly headers?: Readonly<Record<string, string>>;
  /** The error codes the endpoint lists; any other code reads as "http". */
  readonly codes: readonly string[];
}

/**
 * Why a request failed when the server named no code the endpoint lists:
 * an unlisted refusal, an answer the decoder refused, no answer, no answer
 * within requestDeadlineMs, or the caller's signal.
 */
type TransportCode = "http" | "decode" | "network" | "deadline" | "aborted";

interface ApiOk<T> {
  readonly ok: true;
  readonly data: T;
}

interface ApiFailure<E extends string> {
  readonly ok: false;
  /** The answer's status, or 0 when none arrived. */
  readonly status: number;
  readonly code: E | TransportCode;
  /** The server's own words for the refusal, written for a person; null when it sent none. */
  readonly message: string | null;
  /** The answer's Retry-After in ms: how long to wait before sending it again; null when it named none. */
  readonly retryAfterMs: number | null;
}

export type ApiResult<T, E extends string = never> = ApiOk<T> | ApiFailure<E>;

/** A failure's words for a person: the server's own, or that it did not answer. */
export function failureWords(f: { readonly message: string | null }): string {
  return f.message ?? "grelot did not answer. Try again in a moment.";
}

/** How long a request may wait for its whole answer before it fails as "deadline". */
const requestDeadlineMs = 30_000;

/**
 * A new Idempotency-Key value: the RFC 8941 String the server's keyed
 * routes read, quotes included. A re-send of the same post reuses it.
 */
export function newIdempotencyKey(): string {
  return `"${crypto.randomUUID().replaceAll("-", "")}"`;
}

function failure<E extends string>(
  status: number,
  code: E | TransportCode,
  message: string | null,
  retryAfterMs: number | null = null,
): ApiFailure<E> {
  return { ok: false, status, code, message, retryAfterMs };
}

interface Envelope {
  readonly code: string | null;
  readonly message: string | null;
}

/** HTML timers: setTimeout runs a longer delay at once (MDN setTimeout, "Maximum delay value"). */
const longestTimerMs = 2_147_483_647;

const monthNames = "Jan Feb Mar Apr May Jun Jul Aug Sep Oct Nov Dec".split(" ");
const monthPart = `(?<mon>${monthNames.join("|")})`;
const dayPart = "(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun)";
const dayLongPart = "(?:Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|Sunday)";
const timePart = String.raw`(?<h>\d{2}):(?<mi>\d{2}):(?<s>\d{2})`;
// RFC 9110 section 5.6.7's three forms, case-sensitive; asctime has no zone and means GMT.
const imfFixdate = new RegExp(
  String.raw`^${dayPart}, (?<date>\d{2}) ${monthPart} (?<year>\d{4}) ${timePart} GMT$`,
);
const rfc850Date = new RegExp(
  String.raw`^${dayLongPart}, (?<date>\d{2})-${monthPart}-(?<year>\d{2}) ${timePart} GMT$`,
);
const asctimeDate = new RegExp(
  String.raw`^${dayPart} ${monthPart} (?<date>\d{2}| \d) ${timePart} (?<year>\d{4})$`,
);

/** The instant in ms of a UTC date and time, or null when no such date or time exists. */
function utcMs(
  year: number,
  monthIndex: number,
  date: number,
  h: number,
  mi: number,
  s: number,
): number | null {
  // setUTCFullYear, unlike Date.UTC, keeps a year below 100 as written.
  const d = new Date(0);
  d.setUTCFullYear(year, monthIndex, date);
  if (h > 23 || mi > 59 || s > 60 || d.getUTCMonth() !== monthIndex || d.getUTCDate() !== date) {
    return null;
  }
  return d.getTime() + ((h * 60 + mi) * 60 + s) * 1000;
}

/** The instant in ms an HTTP-date names (RFC 9110 section 5.6.7), or null when v is none. */
function httpDateMs(v: string, now: number): number | null {
  const match = imfFixdate.exec(v) ?? rfc850Date.exec(v) ?? asctimeDate.exec(v);
  const { date = "", mon = "", year = "", h = "", mi = "", s = "" } = match?.groups ?? {};
  if (match === null) {
    return null;
  }
  const at = (y: number): number | null =>
    utcMs(y, monthNames.indexOf(mon), Number(date), Number(h), Number(mi), Number(s));
  if (year.length === 4) {
    return at(Number(year));
  }
  // A two-digit year more than 50 years on is the latest past year with those digits.
  const fiftyOn = new Date(now);
  fiftyOn.setUTCFullYear(fiftyOn.getUTCFullYear() + 50);
  const y = Math.floor(new Date(now).getUTCFullYear() / 100) * 100 + Number(year);
  const ms = at(y);
  return ms !== null && ms > fiftyOn.getTime() ? at(y - 100) : ms;
}

/**
 * The wait in ms a Retry-After value names, delta-seconds or an HTTP-date
 * in any of its three forms (RFC 9110 section 10.2.3), held to the longest
 * timer a browser keeps; null when absent or neither. A date passed is 0.
 */
export function retryAfterOf(headers: Headers | undefined): number | null {
  const v = headers?.get("Retry-After") ?? "";
  const now = Date.now();
  const at = /^\d+$/.test(v) ? now + Number(v) * 1000 : httpDateMs(v, now);
  return at === null ? null : Math.min(longestTimerMs, Math.max(0, at - now));
}

/** An error envelope's code and words, read from v as untrusted JSON. */
export function envelopeOf(v: unknown): Envelope {
  if (typeof v !== "object" || v === null || Array.isArray(v)) {
    return { code: null, message: null };
  }
  const read = (k: string): unknown =>
    Object.hasOwn(v, k) ? (v as Record<string, unknown>)[k] : null;
  const code = read("code");
  const error = read("error");
  return {
    code: typeof code === "string" ? code : null,
    message: typeof error === "string" && error !== "" ? error : null,
  };
}

function envelope(text: string): Envelope {
  try {
    return envelopeOf(JSON.parse(text));
  } catch {
    return envelopeOf(null);
  }
}

/**
 * Sends spec same-origin and decodes a 2xx answer with its status, never
 * throwing. A 401 also sends the browser to sign-in, to come back to this
 * address.
 */
export async function clientRequest<T, E extends string>(
  spec: EndpointSpec,
  decode: (v: unknown, status: number) => T,
  opts?: { readonly signal?: AbortSignal },
): Promise<ApiResult<T, E>> {
  const deadline = AbortSignal.timeout(requestDeadlineMs);
  const caller = opts?.signal;
  const signal = caller === undefined ? deadline : AbortSignal.any([caller, deadline]);
  const why = (): TransportCode =>
    caller?.aborted === true ? "aborted" : deadline.aborted ? "deadline" : "network";
  const headers: Record<string, string> = { Accept: "application/json", ...spec.headers };
  if (spec.body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  let response: Response;
  let text = "";
  let blob: Blob | null = null;
  try {
    response = await fetch(spec.path, {
      method: spec.method,
      headers,
      body: spec.body === undefined ? null : JSON.stringify(spec.body),
      credentials: "same-origin",
      signal,
    });
    if (spec.raw === true && response.ok) {
      blob = await response.blob();
    } else {
      text = await response.text();
    }
  } catch {
    return failure(0, why(), null);
  }
  if (response.ok) {
    let body: unknown = blob ?? undefined;
    if (blob === null && text !== "") {
      try {
        body = JSON.parse(text);
      } catch {
        return failure(response.status, "decode", null);
      }
    }
    try {
      return { ok: true, data: decode(body, response.status) };
    } catch {
      return failure(response.status, "decode", null);
    }
  }
  if (response.status === 401) {
    const signIn = signInHref(location.pathname, location.search);
    if (signIn !== null) {
      leaveTo(signIn);
    }
  }
  const { code, message } = envelope(text);
  const listed = code !== null && spec.codes.includes(code) ? (code as E) : "http";
  return failure(response.status, listed, message, retryAfterOf(response.headers));
}
