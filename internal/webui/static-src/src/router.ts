import { signal, type ReadonlySignal } from "@cplieger/reactive/signal";

import type { Show, StatsRange } from "./wire/types.gen.js";

/** The Statistics tab's problem dialogs, one per Needs attention figure. */
type Problems = "refused" | "push" | "nudges";

/** The Settings categories, each its own address. */
export type SettingsSection = "history" | "sign-in";

/** Every address the app answers, as the state it opens. */
export type Route =
  | { readonly screen: "messages"; readonly show: Show; readonly conversation: number | null }
  | { readonly screen: "agent"; readonly name: string }
  | {
      readonly screen: "statistics";
      readonly range: StatsRange;
      readonly problems: Problems | null;
    }
  | { readonly screen: "settings"; readonly section: SettingsSection | null }
  | { readonly screen: "sign-in"; readonly next: string | null }
  | { readonly screen: "setup" }
  | { readonly screen: "recover" }
  | { readonly screen: "not-found"; readonly path: string };

type Screen = Route["screen"];

/** The screens outside the app's frame, which need no session. */
const gateScreens: ReadonlySet<Screen> = new Set(["sign-in", "setup", "recover"]);

/** A parsed address: its route, and what it held that the app does not know. */
export interface Parsed {
  readonly route: Route;
  readonly dropped: readonly string[];
}

const shows: readonly Show[] = ["all", "threads", "direct"];
const ranges: readonly StatsRange[] = ["6h", "24h", "72h"];
const problemKinds: readonly Problems[] = ["refused", "push", "nudges"];
const sections: readonly SettingsSection[] = ["history", "sign-in"];

// The server's name rule (internal/store identity.go namePattern).
const agentName = /^[a-z0-9][a-z0-9-]{1,47}$/;
const conversationID = /^[1-9][0-9]{0,15}$/;

function oneOf<T extends string>(values: readonly T[], v: string): T | null {
  return (values as readonly string[]).includes(v) ? (v as T) : null;
}

/**
 * Reads the query keys a screen knows; every other key, a repeated key and
 * a value outside its set land in dropped, as "key=value" or "key".
 */
function readQuery<K extends string>(
  search: string,
  known: Readonly<Record<K, (v: string) => boolean>>,
  dropped: string[],
): Partial<Record<K, string>> {
  const out: Partial<Record<K, string>> = {};
  for (const [key, value] of new URLSearchParams(search)) {
    if (!Object.hasOwn(known, key)) {
      dropped.push(value === "" ? key : `${key}=${value}`);
      continue;
    }
    const k = key as K;
    if (out[k] !== undefined || !known[k](value)) {
      dropped.push(`${key}=${value}`);
      continue;
    }
    out[k] = value;
  }
  return out;
}

/**
 * A path on this origin, the only place a sign-in may send the browser next.
 * The origin is read from the parsed URL because the URL parser strips tab,
 * LF and CR, which turns "/\t/host" into "//host" (WHATWG URL, basic parser).
 */
function localPath(v: string): boolean {
  if (!v.startsWith("/")) {
    return false;
  }
  try {
    return new URL(v, location.origin).origin === location.origin;
  } catch {
    return false;
  }
}

function segmentsOf(pathname: string): string[] | null {
  try {
    return pathname
      .split("/")
      .filter((s) => s !== "")
      .map((s) => decodeURIComponent(s));
  } catch {
    return null;
  }
}

/**
 * Reads an address. A value the app does not know opens the nearest valid
 * view and is named in dropped; a path no screen answers is not-found.
 */
export function parseRoute(pathname: string, search: string): Parsed {
  const dropped: string[] = [];
  const segments = segmentsOf(pathname);
  const notFound: Parsed = { route: { screen: "not-found", path: pathname }, dropped: [] };
  if (segments === null) {
    return notFound;
  }
  const [first = "messages", second, ...rest] = segments;
  if (rest.length > 0) {
    return notFound;
  }
  switch (first) {
    case "messages": {
      const q = readQuery(search, { show: (v) => oneOf(shows, v) !== null }, dropped);
      let conversation: number | null = null;
      if (second !== undefined) {
        if (conversationID.test(second) && Number.isSafeInteger(Number(second))) {
          conversation = Number(second);
        } else {
          dropped.push(`conversation ${second}`);
        }
      }
      const show = oneOf(shows, q.show ?? "all") ?? "all";
      return { route: { screen: "messages", show, conversation }, dropped };
    }
    case "agents": {
      if (second === undefined) {
        return notFound;
      }
      readQuery(search, {}, dropped);
      if (!agentName.test(second)) {
        dropped.push(`agent ${second}`);
        return { route: { screen: "messages", show: "all", conversation: null }, dropped };
      }
      return { route: { screen: "agent", name: second }, dropped };
    }
    case "statistics": {
      if (second !== undefined) {
        return notFound;
      }
      const q = readQuery(
        search,
        {
          range: (v) => oneOf(ranges, v) !== null,
          problems: (v) => oneOf(problemKinds, v) !== null,
        },
        dropped,
      );
      const range = oneOf(ranges, q.range ?? "24h") ?? "24h";
      const problems = q.problems === undefined ? null : oneOf(problemKinds, q.problems);
      return { route: { screen: "statistics", range, problems }, dropped };
    }
    case "settings": {
      readQuery(search, {}, dropped);
      if (second === undefined) {
        return { route: { screen: "settings", section: null }, dropped };
      }
      const section = oneOf(sections, second);
      if (section === null) {
        dropped.push(`settings ${second}`);
      }
      return { route: { screen: "settings", section }, dropped };
    }
    case "sign-in": {
      if (second !== undefined) {
        return notFound;
      }
      const q = readQuery(search, { next: localPath }, dropped);
      return { route: { screen: "sign-in", next: q.next ?? null }, dropped };
    }
    case "setup":
    case "recover": {
      if (second !== undefined) {
        return notFound;
      }
      readQuery(search, {}, dropped);
      return { route: { screen: first }, dropped };
    }
    default:
      return notFound;
  }
}

function withQuery(path: string, entries: readonly (readonly [string, string | null])[]): string {
  const q = new URLSearchParams();
  for (const [k, v] of entries) {
    if (v !== null) {
      q.append(k, v);
    }
  }
  const s = q.toString();
  return s === "" ? path : `${path}?${s}`;
}

/** The canonical address of a route: parseRoute(routeHref(r)) gives r back. */
export function routeHref(route: Route): string {
  switch (route.screen) {
    case "messages": {
      const path =
        route.conversation === null ? "/messages" : `/messages/${String(route.conversation)}`;
      return withQuery(path, [["show", route.show === "all" ? null : route.show]]);
    }
    case "agent":
      return `/agents/${encodeURIComponent(route.name)}`;
    case "statistics":
      return withQuery("/statistics", [
        ["range", route.range === "24h" ? null : route.range],
        ["problems", route.problems],
      ]);
    case "settings":
      return route.section === null ? "/settings" : `/settings/${route.section}`;
    case "sign-in":
      return withQuery("/sign-in", [["next", route.next]]);
    case "setup":
      return "/setup";
    case "recover":
      return "/recover";
    case "not-found":
      return route.path;
  }
}

/** The agent page of name, or null for a name no agent page has, such as a delegate's handle. */
export function agentHref(name: string): string | null {
  return agentName.test(name) ? routeHref({ screen: "agent", name }) : null;
}

/**
 * The sign-in address that brings the browser back to pathname and search,
 * or null when that address is itself a gate page, so a refused request
 * there never loops.
 */
export function signInHref(pathname: string, search: string): string | null {
  const { route } = parseRoute(pathname, search);
  if (gateScreens.has(route.screen)) {
    return null;
  }
  return routeHref({
    screen: "sign-in",
    next: route.screen === "not-found" ? null : pathname + search,
  });
}

/** How the current route was reached. */
type Cause = "load" | "push" | "replace" | "pop";

/** The current route, how it was reached and what an entry left to restore. */
export interface Located extends Parsed {
  readonly cause: Cause;
  /** The window's scroll when the reader left this entry; null for a new one. */
  readonly scroll: number | null;
  /** The href of the link that had focus when the reader left this entry. */
  readonly focus: string | null;
}

/** Work a move away would discard, such as unsaved settings. */
interface Hold {
  /** Whether opening next would discard the work. */
  readonly holds: (next: Route) => boolean;
  /** Asks the reader whether to leave; resolves true to leave. */
  readonly ask: () => Promise<boolean>;
}

export interface Router {
  readonly current: ReadonlySignal<Located>;
  /** Opens route: a new history entry, or the current one replaced. */
  go(route: Route, opts?: { readonly replace?: boolean }): void;
  /**
   * Asks hold before each move it holds, a go, a link or Back, until
   * lifetime aborts; a declined move leaves the address as it was.
   */
  hold(hold: Hold, lifetime: AbortSignal): void;
}

interface EntryState {
  readonly scroll: number;
  readonly focus: string | null;
}

function entryState(v: unknown): EntryState | null {
  if (typeof v !== "object" || v === null) {
    return null;
  }
  const scroll: unknown = Object.hasOwn(v, "scroll")
    ? (v as Record<string, unknown>)["scroll"]
    : null;
  const focus: unknown = Object.hasOwn(v, "focus") ? (v as Record<string, unknown>)["focus"] : null;
  if (typeof scroll !== "number" || !Number.isFinite(scroll)) {
    return null;
  }
  return { scroll, focus: typeof focus === "string" ? focus : null };
}

function focusedHref(): string | null {
  const active = document.activeElement;
  return active instanceof HTMLAnchorElement ? active.getAttribute("href") : null;
}

/** Records where the reader is on the entry about to be left. */
function remember(): void {
  const state: EntryState = { scroll: window.scrollY, focus: focusedHref() };
  history.replaceState(state, "");
}

/**
 * Follows the address: canonicalises it on load (replacing an address that
 * held unknown values), takes over same-origin links to the app's own
 * screens, and reads Back and Forward. Gate pages are full loads.
 */
export function startRouter(lifetime: AbortSignal): Router {
  history.scrollRestoration = "manual";
  const here = (): Parsed => parseRoute(location.pathname, location.search);
  const first = here();
  const canonical = routeHref(first.route);
  if (canonical !== location.pathname + location.search) {
    history.replaceState(history.state, "", canonical);
  }
  const current = signal<Located>({ ...first, cause: "load", scroll: null, focus: null });
  let held: Hold | null = null;
  // Set while history.back() returns to an entry Back had left and the hold let go.
  let released = false;

  const move = (route: Route, opts?: { readonly replace?: boolean }): void => {
    const href = routeHref(route);
    const replace = opts?.replace === true;
    if (replace) {
      history.replaceState(history.state, "", href);
    } else {
      if (href === location.pathname + location.search) {
        return;
      }
      remember();
      history.pushState(null, "", href);
    }
    current.value = {
      route,
      dropped: [],
      cause: replace ? "replace" : "push",
      scroll: null,
      focus: null,
    };
  };

  const go = (route: Route, opts?: { readonly replace?: boolean }): void => {
    const h = held;
    if (h?.holds(route) !== true) {
      move(route, opts);
      return;
    }
    void h.ask().then((leave) => {
      if (leave) {
        move(route, opts);
      }
    });
  };

  const hold = (h: Hold, lifetime: AbortSignal): void => {
    if (lifetime.aborted) {
      return;
    }
    held = h;
    lifetime.addEventListener(
      "abort",
      () => {
        if (held === h) {
          held = null;
        }
      },
      { once: true },
    );
  };

  window.addEventListener(
    "popstate",
    (event) => {
      const next = here();
      const h = held;
      if (!released && h?.holds(next.route) === true) {
        // The browser has already moved, so the address goes back to the
        // held screen's as a new entry, and a yes returns to where Back went.
        history.pushState(null, "", routeHref(current.peek().route));
        void h.ask().then((leave) => {
          if (leave) {
            released = true;
            history.back();
          }
        });
        return;
      }
      released = false;
      const saved = entryState(event.state);
      current.value = {
        ...next,
        cause: "pop",
        scroll: saved?.scroll ?? null,
        focus: saved?.focus ?? null,
      };
    },
    { signal: lifetime },
  );

  // Forward needs the entry Back left, whose state popstate no longer
  // reaches, so the current entry is kept up to date as the reader moves.
  let settle: ReturnType<typeof setTimeout> | undefined;
  window.addEventListener(
    "scroll",
    () => {
      clearTimeout(settle);
      settle = setTimeout(remember, 250);
    },
    { signal: lifetime, passive: true },
  );
  document.addEventListener("focusin", remember, { signal: lifetime });
  lifetime.addEventListener("abort", () => {
    clearTimeout(settle);
  });

  document.addEventListener(
    "click",
    (event) => {
      if (event.defaultPrevented || event.button !== 0) {
        return;
      }
      if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
        return;
      }
      const link = event.target instanceof Element ? event.target.closest("a[href]") : null;
      if (!(link instanceof HTMLAnchorElement) || link.hasAttribute("download")) {
        return;
      }
      if (link.target !== "" && link.target !== "_self") {
        return;
      }
      const url = new URL(link.href);
      if (url.origin !== location.origin || url.hash !== "") {
        return;
      }
      const parsed = parseRoute(url.pathname, url.search);
      if (gateScreens.has(parsed.route.screen) || parsed.route.screen === "not-found") {
        return;
      }
      event.preventDefault();
      go(parsed.route);
    },
    { signal: lifetime },
  );

  return { current, go, hold };
}
