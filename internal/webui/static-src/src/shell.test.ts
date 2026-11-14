import { signal } from "@cplieger/reactive/signal";
import { installAnnouncer } from "@cplieger/ui/announce";
import { createTheme } from "@cplieger/ui/theme";
import { http, HttpResponse } from "msw";
import { setupWorker } from "msw/browser";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

import { meAnswer } from "./__test-helpers__/me.js";
import { until } from "./__test-helpers__/until.js";
import type { Connection, Live } from "./live.js";
import { leaveTo } from "./location.js";
import { startRouter, type Route } from "./router.js";
import { startShell, statusWords, type ScreenMount, type Screens } from "./shell.js";

vi.mock("./location.js", () => ({ leaveTo: vi.fn() }));

const worker = setupWorker();
const originalHref = location.href;
let page = new AbortController();
let root: HTMLElement;
let connection = signal<Connection>({ kind: "live" });
let reconnects = 0;
let mounts: string[] = [];
let updates: Route[] = [];

const live: Live = {
  get connection() {
    return connection;
  },
  watch: () => undefined,
  reconnect: () => {
    reconnects++;
  },
};

function screen<S extends "messages" | "agent" | "statistics" | "settings">(
  heading: string,
  updatable = false,
): ScreenMount<S> {
  return (host) => {
    mounts.push(heading);
    const h1 = document.createElement("h1");
    h1.textContent = heading;
    host.append(h1);
    return updatable ? { update: (r) => void updates.push(r) } : {};
  };
}

const screens: Screens = {
  messages: screen("Messages", true),
  agent: screen("Agent"),
  statistics: screen("Statistics"),
  settings: screen("Settings"),
};

function start(href: string): ReturnType<typeof startRouter> {
  history.replaceState(null, "", href);
  const router = startRouter(page.signal);
  const resolvedTheme = signal<"light" | "dark">("light");
  const theme = createTheme({
    storageKey: "grelot.test.theme",
    storage: { get: () => null, set: () => undefined },
    signal: page.signal,
  });
  startShell({ root, router, live, screens, theme, resolvedTheme, signal: page.signal });
  return router;
}

const header = (): HTMLElement => root.querySelector("header") as HTMLElement;
const navLink = (word: string): HTMLElement =>
  [...header().querySelectorAll<HTMLElement>("a")].find(
    (a) => a.textContent === word || a.getAttribute("aria-label") === word,
  ) as HTMLElement;
const statusButton = (): HTMLButtonElement =>
  root.querySelector(".cp-header-status") as HTMLButtonElement;

beforeAll(async () => {
  await worker.start({ quiet: true, onUnhandledFrame: "error" });
});

beforeEach(() => {
  root = document.createElement("div");
  document.body.append(root);
  installAnnouncer({ signal: page.signal });
  connection = signal<Connection>({ kind: "live" });
  reconnects = 0;
  mounts = [];
  updates = [];
  // Reset here rather than after each test: the account request a test
  // started can still reach the worker after its page has gone.
  worker.resetHandlers();
  worker.use(http.get("/api/auth/me", () => HttpResponse.json(meAnswer("mika", "Ana Mika"))));
});

afterEach(() => {
  document.documentElement.removeAttribute("data-theme");
  page.abort();
  page = new AbortController();
  document.body.replaceChildren();
  history.replaceState(null, "", originalHref);
});

afterAll(() => {
  worker.stop();
});

describe("startShell", () => {
  it("marks the destination the route sits in, and the title names the screen", async () => {
    const router = start("/messages");
    expect(navLink("Messages").getAttribute("aria-current")).toBe("page");
    expect(document.title).toBe("Messages · grelot");

    router.go({ screen: "agent", name: "kiro" });
    await until(() => {
      expect(document.title).toBe("@kiro · grelot");
    });
    expect(navLink("Messages").getAttribute("aria-current")).toBe("true");

    router.go({ screen: "statistics", range: "24h", problems: null });
    await until(() => {
      expect(navLink("Statistics").getAttribute("aria-current")).toBe("page");
    });
    expect(navLink("Messages").hasAttribute("aria-current")).toBe(false);

    router.go({ screen: "settings", section: "history" });
    await until(() => {
      expect(navLink("Settings").getAttribute("aria-current")).toBe("true");
    });
    expect(navLink("Statistics").hasAttribute("aria-current")).toBe(false);
  });

  it("leaves focus alone on load and moves it to the new screen's heading on a route change", async () => {
    const router = start("/messages");
    expect(document.activeElement).toBe(document.body);
    router.go({ screen: "statistics", range: "24h", problems: null });
    await until(() => {
      expect(document.activeElement?.textContent).toBe("Statistics");
    });
    expect(document.activeElement?.tagName).toBe("H1");
  });

  it("hands a route of the same screen to the mounted screen instead of building it again", async () => {
    const router = start("/messages");
    const heading = root.querySelector("main h1");
    router.go({ screen: "messages", show: "direct", conversation: null });
    await until(() => {
      expect(updates).toEqual([{ screen: "messages", show: "direct", conversation: null }]);
    });
    expect(mounts).toEqual(["Messages"]);
    expect(root.querySelector("main h1")).toBe(heading);
  });

  it("builds a screen that cannot update itself again for its next route", async () => {
    const router = start("/agents/ana");
    router.go({ screen: "agent", name: "kiro" });
    await until(() => {
      expect(mounts).toEqual(["Agent", "Agent"]);
    });
    expect(root.querySelectorAll("main h1")).toHaveLength(1);
  });

  it("gives a screen a rail before main that leaves with the screen", async () => {
    const withRail: ScreenMount<"messages"> = (host, _route, ctx) => {
      ctx.rail("Filters").append(document.createElement("p"));
      host.append(document.createElement("h1"));
      return {};
    };
    history.replaceState(null, "", "/messages");
    const router = startRouter(page.signal);
    startShell({
      root,
      router,
      live,
      screens: { ...screens, messages: withRail },
      theme: createTheme({
        storageKey: "grelot.test.theme",
        storage: { get: () => null, set: () => undefined },
        signal: page.signal,
      }),
      resolvedTheme: signal<"light" | "dark">("light"),
      signal: page.signal,
    });
    const rail = root.querySelector("aside.cp-rail");
    expect(rail?.getAttribute("aria-label")).toBe("Filters");
    expect(rail?.nextElementSibling).toBe(root.querySelector("main"));
    expect(rail?.parentElement?.classList.contains("cp-app")).toBe(true);

    router.go({ screen: "statistics", range: "24h", problems: null });
    await until(() => {
      expect(mounts).toEqual(["Statistics"]);
    });
    expect(root.querySelector("aside.cp-rail")).toBeNull();
  });

  it("says what an address held that grelot does not know", () => {
    start("/messages?show=bogus&x=1");
    const notice = root.querySelector("main .cp-notice") as HTMLElement;
    expect(notice.hidden).toBe(false);
    expect(notice.textContent).toBe(
      "This address held show=bogus, x=1, which grelot does not know, so it was left out.",
    );
  });

  it("answers an address no screen has with a way back to Messages", () => {
    start("/nope");
    expect(root.querySelector("main h1")?.textContent).toBe("This page does not exist");
    expect(root.querySelector("main a.cp-button")?.getAttribute("href")).toBe("/messages");
    expect(document.title).toBe("Page not found · grelot");
    expect(mounts).toEqual([]);
  });

  it("names the signed-in account on its button and in its menu", async () => {
    start("/messages");
    const button = root.querySelector(".cp-account") as HTMLElement;
    await until(() => {
      expect(button.getAttribute("aria-label")).toBe("Account: mika");
    });
    expect(button.textContent).toBe("A");
    expect(root.querySelector("#account-menu .cp-menu-label")?.textContent).toBe(
      "Signed in as mika",
    );
  });

  it("signs out and leaves for the sign-in page", async () => {
    let posted = 0;
    worker.use(
      http.post("/api/auth/logout", () => {
        posted++;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    start("/messages");
    (root.querySelector(".cp-account") as HTMLElement).click();
    const item = [...root.querySelectorAll<HTMLElement>('#account-menu [role="menuitem"]')].find(
      (b) => b.textContent === "Sign out",
    ) as HTMLElement;
    item.click();
    await until(() => {
      expect(leaveTo).toHaveBeenCalledExactlyOnceWith("/sign-in");
    });
    expect(posted).toBe(1);
  });

  it("keeps the person here with a Retry when signing out fails", async () => {
    worker.use(
      http.post("/api/auth/logout", () =>
        HttpResponse.json(
          { error: "the account store is unavailable", code: "auth_unavailable" },
          { status: 503 },
        ),
      ),
    );
    start("/messages");
    (root.querySelector(".cp-account") as HTMLElement).click();
    (root.querySelector('#account-menu [role="menuitem"]') as HTMLElement).click();
    await until(() => {
      expect(document.body.textContent).toContain("Sign out did not finish.");
    });
    expect(document.body.textContent).toContain("the account store is unavailable");
    expect(leaveTo).not.toHaveBeenCalled();
  });

  it("says grelot did not answer when signing out gets no answer", async () => {
    worker.use(http.post("/api/auth/logout", () => HttpResponse.error()));
    start("/messages");
    (root.querySelector(".cp-account") as HTMLElement).click();
    (root.querySelector('#account-menu [role="menuitem"]') as HTMLElement).click();
    await until(() => {
      expect(document.body.textContent).toContain("Sign out did not finish.");
    });
    expect(document.body.textContent).toContain("grelot did not answer. Try again in a moment.");
    expect(leaveTo).not.toHaveBeenCalled();
  });

  it("sends a browser whose session ended to sign-in, to come back here", async () => {
    worker.use(
      http.get("/api/auth/me", () =>
        HttpResponse.json(
          { error: "the session has expired", code: "auth_session_expired" },
          { status: 401 },
        ),
      ),
    );
    start("/messages/7?show=direct");
    await until(() => {
      expect(leaveTo).toHaveBeenCalledExactlyOnceWith(
        "/sign-in?next=%2Fmessages%2F7%3Fshow%3Ddirect",
      );
    });
  });

  it("shows the header status only while the stream is not live, and says each change", async () => {
    start("/messages");
    expect(statusButton().hidden).toBe(true);

    connection.value = { kind: "reconnecting", since: null };
    expect(statusButton().hidden).toBe(false);
    expect(statusButton().getAttribute("data-state")).toBe("working");
    expect(statusButton().textContent).toBe("Reconnecting…");
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "Reconnecting to grelot.",
      );
    });

    const since = new Date();
    since.setHours(14, 2, 0, 0);
    connection.value = { kind: "offline", since: since.getTime() };
    expect(statusButton().getAttribute("data-state")).toBe("offline");
    expect(statusButton().textContent).toMatch(/^OfflineShowing what was loaded at .+\.$/);

    connection.value = { kind: "live" };
    expect(statusButton().hidden).toBe(true);
    await until(() => {
      expect(document.body.querySelector('[aria-live="polite"]')?.textContent).toBe(
        "Connected again.",
      );
    });
  });

  it("turns the theme from its toggle, and follows a change made elsewhere", async () => {
    history.replaceState(null, "", "/messages");
    const resolvedTheme = signal<"light" | "dark">("light");
    let stored: string | null = null;
    const theme = createTheme({
      storageKey: "grelot.test.theme",
      storage: { get: () => stored, set: (v) => void (stored = v) },
      signal: page.signal,
      onChange: (r) => {
        resolvedTheme.value = r;
      },
    });
    startShell({
      root,
      router: startRouter(page.signal),
      live,
      screens,
      theme,
      resolvedTheme,
      signal: page.signal,
    });
    const toggle = root.querySelector('[data-kind="theme"]') as HTMLButtonElement;
    toggle.click();
    await until(() => {
      expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    });
    expect(stored).toBe("dark");
    expect(toggle.getAttribute("aria-pressed")).toBe("true");
    resolvedTheme.value = "light";
    expect(toggle.getAttribute("aria-pressed")).toBe("false");
  });

  it("offers Try now while reconnecting, which connects again at once", () => {
    start("/messages");
    connection.value = { kind: "reconnecting", since: null };
    const tryNow = [...root.querySelectorAll<HTMLButtonElement>("#status-panel button")].find(
      (b) => b.textContent === "Try now",
    ) as HTMLButtonElement;
    expect(tryNow.hidden).toBe(false);
    tryNow.click();
    expect(reconnects).toBe(1);
    connection.value = { kind: "offline", since: null };
    expect(tryNow.hidden).toBe(true);
  });
});

describe("statusWords", () => {
  const now = new Date(2026, 10, 13, 15, 0).getTime();
  const at = new Date(2026, 10, 13, 14, 2).getTime();

  it("says what the page shows while offline, and when it was loaded", () => {
    const words = statusWords({ kind: "offline", since: at }, now);
    expect(words.label).toBe("Offline");
    expect(words.detail).toMatch(
      /^Showing what was loaded at 14:02|^Showing what was loaded at 2:02/,
    );
    expect(words.announced).toBe(`Offline. ${words.detail ?? ""}`);
  });

  it("says nothing about a load time it does not have", () => {
    expect(statusWords({ kind: "offline", since: null }, now)).toMatchObject({
      detail: null,
      announced: "Offline.",
    });
    expect(statusWords({ kind: "reconnecting", since: null }, now).sentence).toBe(
      "What you see may be out of date, and the page keeps trying.",
    );
  });
});
