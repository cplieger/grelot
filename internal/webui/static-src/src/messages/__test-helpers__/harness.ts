import { signal } from "@cplieger/reactive/signal";
import { installAnchors } from "@cplieger/ui/anchor";
import { installAnnouncer } from "@cplieger/ui/announce";
import { installDialogs } from "@cplieger/ui/dialog";
import { createTheme } from "@cplieger/ui/theme";
import { http, HttpResponse } from "msw";
import { setupWorker } from "msw/browser";
import { afterAll, afterEach, beforeAll, beforeEach, expect } from "vitest";

import { meAnswer } from "../../__test-helpers__/me.js";
import { until } from "../../__test-helpers__/until.js";
import type { Connection, Live } from "../../live.js";
import { startRouter } from "../../router.js";
import { startShell, type FrameScreen, type ScreenMount, type Screens } from "../../shell.js";
import type {
  Changed,
  Conversation,
  ConversationRow,
  ConversationsPage,
  Thread,
} from "../../wire/types.gen.js";
import { mountAgent } from "../agent.js";
import { messagesScreen } from "../screen.js";

type Watcher = Parameters<Live["watch"]>[0];
type Answer = () => Response | Promise<Response>;

export const worker = setupWorker();
export const hour = 3_600_000;
export const at = Date.parse("2026-11-13T09:40:00Z");

/** What the Messages tests share: the page, the fake stream and the fake server's answers. */
export const h = {
  page: new AbortController(),
  root: document.createElement("div"),
  watchers: [] as Watcher[],
  /** The query of each /api/conversations request, in order. */
  asked: [] as URLSearchParams[],
  /** The answers /api/conversations gives in turn; the last one repeats. */
  answers: [] as Answer[],
  /** Each listed row's thread, which /api/conversations/{id} answers by default. */
  threads: new Map<number, Thread>(),
};

const originalHref = location.href;

const live: Live = {
  connection: signal<Connection>({ kind: "live" }),
  watch: (w, lifetime) => {
    h.watchers.push(w);
    lifetime.addEventListener("abort", () => {
      h.watchers = h.watchers.filter((x) => x !== w);
    });
  },
  reconnect: () => undefined,
};

export function row(id: number, over: Partial<ConversationRow> = {}): ConversationRow {
  const r: ConversationRow = {
    id,
    kind: "path",
    title: `/workspace/r${String(id)}`,
    path: `/workspace/r${String(id)}`,
    last: { id: id * 10, from: "bravo", subject: "s", preview: `p${String(id)}`, at_ms: at },
    participants: 2,
    unread: 0,
    ...over,
  };
  const { kind, title, path, agents } = r;
  h.threads.set(id, {
    id,
    kind,
    title,
    ...(path === undefined ? {} : { path }),
    ...(agents === undefined ? {} : { agents }),
  });
  return r;
}

export function pageOf(
  rows: ConversationRow[],
  over: Partial<ConversationsPage> = {},
): ConversationsPage {
  return { rows, counts: { threads: rows.length, direct: 0 }, ...over };
}

export function answer(...pages: ConversationsPage[]): void {
  h.answers = pages.map((p) => () => HttpResponse.json(p));
}

/** A conversation with nothing around it, for the thread of row id. */
export function conversationOf(id: number, over: Partial<Conversation> = {}): Conversation {
  const thread = h.threads.get(id) ?? { id, kind: "path", title: `/workspace/r${String(id)}` };
  return { thread, posts: [], reservations: [], followers: [], cards: [], ...over };
}

/** Starts the app at href; screens replaces any screen, such as the pending Statistics. */
export function start(
  href: string,
  screens: Partial<Screens> = {},
): ReturnType<typeof startRouter> {
  history.replaceState(null, "", href);
  const router = startRouter(h.page.signal);
  const pending =
    <S extends FrameScreen>(): ScreenMount<S> =>
    (host) => {
      host.append(document.createElement("h1"));
      return {};
    };
  startShell({
    root: h.root,
    router,
    live,
    theme: createTheme({
      storageKey: "grelot.test.theme",
      storage: { get: () => null, set: () => undefined },
      signal: h.page.signal,
    }),
    resolvedTheme: signal<"light" | "dark">("light"),
    signal: h.page.signal,
    screens: {
      messages: messagesScreen(),
      agent: mountAgent,
      statistics: pending(),
      settings: pending(),
      ...screens,
    },
  });
  return router;
}

/** Ends the page and clears it, so a test can start a second one. */
export function restart(): void {
  h.page.abort();
  h.page = new AbortController();
  h.root.replaceChildren();
}

/** The live stream reports change to every open view. */
export function emit(change: Changed): void {
  for (const w of h.watchers) {
    w.changed?.(change);
  }
}

export function button(word: string, within: ParentNode = h.root): HTMLButtonElement {
  const found = [...within.querySelectorAll<HTMLButtonElement>("button")].find(
    (b) => b.textContent === word,
  );
  if (found === undefined) {
    throw new Error(`no button reads ${word}`);
  }
  return found;
}

/**
 * The button reading word once it is at rest: a busy button reads its busy
 * words for at least busyMin after slow work, which a loaded runner hits.
 */
export function rested(word: string, within: ParentNode = h.root): Promise<HTMLButtonElement> {
  return until(() => button(word, within));
}

export const links = (): HTMLAnchorElement[] => [
  ...h.root.querySelectorAll<HTMLAnchorElement>("a.cp-list-row"),
];

export const titles = (): string[] =>
  links().map((a) => a.querySelector(".cp-truncate")?.textContent ?? "");

export async function rowsShown(want: string[]): Promise<void> {
  await until(() => {
    expect(titles()).toEqual(want);
  });
}

/** Registers the hooks every Messages test file runs under. */
export function useMessagesHarness(): void {
  beforeAll(async () => {
    await worker.start({ quiet: true, onUnhandledFrame: "error" });
  });

  beforeEach(() => {
    h.root = document.createElement("div");
    document.body.append(h.root);
    installAnnouncer({ signal: h.page.signal });
    installDialogs({ signal: h.page.signal });
    installAnchors({ signal: h.page.signal });
    h.watchers = [];
    h.asked = [];
    h.threads = new Map();
    answer(pageOf([]));
    worker.resetHandlers();
    worker.use(
      http.get("/api/auth/me", () => HttpResponse.json(meAnswer("mika"))),
      http.get("/api/settings", () =>
        HttpResponse.json({
          retention_ms: 72 * hour,
          retention_min_ms: hour,
          retention_max_ms: 2160 * hour,
          methods: { password: true, passkeys: true, oidc: false },
          oidc_configured: false,
        }),
      ),
      http.get("/api/agents", () =>
        HttpResponse.json({
          agents: [
            {
              name: "alpha",
              description: "d",
              state: "live",
              paths: [],
              follows: [],
              registered_ms: at,
            },
          ],
        }),
      ),
      http.get("/api/conversations", ({ request }) => {
        h.asked.push(new URL(request.url).searchParams);
        const next = h.answers.length > 1 ? h.answers.shift() : h.answers[0];
        return next === undefined ? HttpResponse.json(pageOf([])) : next();
      }),
      http.get("/api/conversations/:id", ({ params }) => {
        const id = Number(params["id"]);
        return h.threads.has(id)
          ? HttpResponse.json(conversationOf(id))
          : HttpResponse.json(
              { error: "no such conversation", code: "not_found" },
              { status: 404 },
            );
      }),
      http.post("/api/conversations/:id/read", () => HttpResponse.json({ read: 0 })),
    );
  });

  afterEach(() => {
    sessionStorage.clear();
    h.page.abort();
    h.page = new AbortController();
    document.body.replaceChildren();
    history.replaceState(null, "", originalHref);
  });

  afterAll(() => {
    void worker.stop();
  });
}
