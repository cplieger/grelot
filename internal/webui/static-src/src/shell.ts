import { el } from "@cplieger/reactive/dom";
import {
  createRoot,
  effect,
  runWithOwner,
  untracked,
  type Owner,
  type ReadonlySignal,
} from "@cplieger/reactive/signal";
import { announce } from "@cplieger/ui/announce";
import { enhanceMenu } from "@cplieger/ui/menu";
import { enhancePanel } from "@cplieger/ui/panel";
import type { ThemeController } from "@cplieger/ui/theme";
import { toast } from "@cplieger/ui/toast";
import { enhanceToggle } from "@cplieger/ui/toggle";
import { viewTransition } from "@cplieger/ui/view-transition";

import { fetchAccount, signOut, type Account } from "./account.js";
import { clockTime } from "./format.js";
import { icon } from "./icons.js";
import type { Connection, Live } from "./live.js";
import { leaveTo } from "./location.js";
import { routeHref, type Located, type Route, type Router } from "./router.js";
import { failureWords } from "./transport.js";

/** What a screen gets for its lifetime. */
interface ScreenContext {
  /** Aborts when the screen leaves the page. */
  readonly signal: AbortSignal;
  /** Aborts when the page ends: for work that outlives the screen, such as a send. */
  readonly page: AbortSignal;
  readonly live: Live;
  readonly router: Router;
  /**
   * The frame's rail, named label, empty for the screen to fill; it leaves
   * with the screen. The stylesheet hides it below 60rem.
   */
  rail(label: string): HTMLElement;
}

/** A mounted screen. update takes a new route of the same screen in place. */
interface ScreenView {
  readonly update?: (route: Route) => void;
}

/** The screens inside the frame; not-found is the shell's own. */
export type FrameScreen = "messages" | "agent" | "statistics" | "settings";

/**
 * Renders one screen into host. It renders its <h1> before it returns, so
 * the shell can move focus to it, and takes ctx.signal for every listener
 * and request that ends with the screen.
 */
export type ScreenMount<S extends FrameScreen> = (
  host: HTMLElement,
  route: Extract<Route, { readonly screen: S }>,
  ctx: ScreenContext,
) => ScreenView;

export type Screens = { readonly [S in FrameScreen]: ScreenMount<S> };

export interface ShellOptions {
  readonly root: HTMLElement;
  readonly router: Router;
  readonly live: Live;
  readonly screens: Screens;
  readonly theme: ThemeController;
  /** The resolved theme, which another tab or the OS can change. */
  readonly resolvedTheme: ReadonlySignal<"light" | "dark">;
  readonly signal: AbortSignal;
}

/** The <title> of each route, before " · grelot". */
export function screenTitle(route: Route): string {
  switch (route.screen) {
    case "messages":
      return "Messages";
    case "agent":
      return `@${route.name}`;
    case "statistics":
      return "Statistics";
    case "settings":
      return "Settings";
    case "sign-in":
      return "Sign in";
    case "setup":
      return "Set up grelot";
    case "recover":
      return "Recover your account";
    case "not-found":
      return "Page not found";
  }
}

/** Which header destination a route sits in, and whether it is that destination itself. */
function currentIn(route: Route, dest: "messages" | "statistics" | "settings"): string | null {
  switch (route.screen) {
    case "messages":
      if (dest !== "messages") {
        return null;
      }
      return route.conversation === null ? "page" : "true";
    case "agent":
      return dest === "messages" ? "true" : null;
    case "statistics":
      return dest === "statistics" ? "page" : null;
    case "settings":
      if (dest !== "settings") {
        return null;
      }
      return route.section === null ? "page" : "true";
    default:
      return null;
  }
}

function setCurrent(link: HTMLElement, value: string | null): void {
  if (value === null) {
    link.removeAttribute("aria-current");
  } else {
    link.setAttribute("aria-current", value);
  }
}

/** The header status's words for a connection that is not live. */
export function statusWords(
  c: Exclude<Connection, { kind: "live" }>,
  now: number = Date.now(),
): {
  readonly label: string;
  readonly detail: string | null;
  readonly heading: string;
  readonly fact: string;
  readonly sentence: string;
  readonly announced: string;
} {
  const loaded = c.since === null ? null : clockTime(c.since, now);
  if (c.kind === "offline") {
    const detail = loaded === null ? null : `Showing what was loaded at ${loaded}.`;
    return {
      label: "Offline",
      detail,
      heading: "Offline",
      fact: "This device is offline",
      sentence:
        loaded === null
          ? "The page reconnects once the network is back."
          : `What you see was loaded at ${loaded}. The page reconnects once the network is back.`,
      announced: detail === null ? "Offline." : `Offline. ${detail}`,
    };
  }
  return {
    label: "Reconnecting…",
    detail: null,
    heading: "Reconnecting…",
    fact: "grelot is not answering",
    sentence:
      loaded === null
        ? "What you see may be out of date, and the page keeps trying."
        : `Since ${loaded}. What you see was loaded then, and the page keeps trying.`,
    announced: "Reconnecting to grelot.",
  };
}

function initials(account: Account): string {
  const [first = ""] = account.displayName ?? account.username;
  return first.toLocaleUpperCase();
}

/**
 * Draws the app's frame (header, main, status) into root and renders the
 * router's current screen into main, keeping the header's destinations,
 * the title, the focus and the scroll right on each route change.
 */
export function startShell(opts: ShellOptions): void {
  createRoot((dispose, owner) => {
    opts.signal.addEventListener("abort", dispose, { once: true });
    frame(opts, owner);
  });
}

function frame(opts: ShellOptions, owner: Owner): void {
  const { router, live, screens, signal } = opts;

  const navItem = (
    href: string,
    iconName: "messages-square" | "chart-column",
    word: string,
  ): HTMLElement =>
    el(
      "a",
      { className: "cp-nav-item", href, "data-cp-tip": word },
      icon(iconName),
      el("span", { className: "cp-nav-label" }, word),
    );
  const messagesLink = navItem("/messages", "messages-square", "Messages");
  const statisticsLink = navItem("/statistics", "chart-column", "Statistics");
  const settingsLink = el(
    "a",
    {
      className: "cp-button",
      "data-variant": "quiet",
      href: "/settings",
      "aria-label": "Settings",
      "data-cp-tip": "Settings",
    },
    icon("settings"),
  );

  const statusLabel = el("span");
  const statusDetail = el("span", { "data-part": "detail" });
  const statusButton = el(
    "button",
    { className: "cp-header-status", type: "button", popovertarget: "status-panel", hidden: true },
    statusLabel,
    statusDetail,
  ) as HTMLButtonElement;
  const panelHeading = el("h2");
  const panelFact = el("strong");
  const panelSentence = el("p");
  const tryNow = el("button", { className: "cp-button", type: "button" }, "Try now");
  tryNow.addEventListener(
    "click",
    () => {
      live.reconnect();
    },
    { signal },
  );
  const panel = el(
    "div",
    {
      className: "cp-popover cp-status-panel",
      "data-variant": "panel",
      popover: "auto",
      id: "status-panel",
    },
    panelHeading,
    el("section", null, panelFact, panelSentence, tryNow),
  );

  const themeButton = el(
    "button",
    {
      className: "cp-button cp-toggle",
      "data-variant": "quiet",
      "data-kind": "theme",
      type: "button",
      "aria-label": "Dark theme",
      "data-cp-tip": "Dark theme",
      "aria-pressed": String(opts.resolvedTheme.peek() === "dark"),
    },
    icon("sun"),
    (() => {
      const on = icon("moon");
      on.setAttribute("data-part", "on");
      return on;
    })(),
  ) as HTMLButtonElement;
  const themeToggle = enhanceToggle(themeButton, {
    signal,
    onChange: (on) => {
      void viewTransition(
        () => {
          opts.theme.set(on ? "dark" : "light");
        },
        { from: themeButton },
      );
    },
  });

  const avatar = el("span", { className: "cp-avatar", "data-size": "sm", "aria-hidden": "true" });
  const accountButton = el(
    "button",
    {
      className: "cp-button cp-account",
      "data-variant": "quiet",
      type: "button",
      "aria-label": "Account",
      popovertarget: "account-menu",
    },
    avatar,
  );
  const signedInAs = el("div", { className: "cp-menu-label" }, "Signed in");
  const signOutItem = el(
    "button",
    { role: "menuitem", type: "button" },
    icon("log-out"),
    "Sign out",
  );
  const accountMenu = el(
    "div",
    { className: "cp-popover cp-menu", popover: "auto", id: "account-menu", role: "menu" },
    signedInAs,
    el("hr"),
    signOutItem,
  );

  const notice = el("div", { className: "cp-notice", role: "status", hidden: true });
  const host = el("div");
  const main = el("main", { className: "cp-main", id: "main", tabIndex: -1 }, notice, host);

  opts.root.replaceChildren(
    el("a", { className: "cp-skip", href: "#main" }, "Skip to content"),
    el(
      "div",
      { className: "cp-app" },
      el(
        "header",
        { className: "cp-header" },
        el(
          "a",
          { className: "cp-wordmark", href: "/messages", "aria-label": "grelot" },
          el("span", { className: "cp-mark", "aria-hidden": "true" }, "g"),
          el("span", { className: "cp-hidden-narrow" }, "grelot"),
        ),
        el("nav", { className: "cp-nav", "aria-label": "Main" }, messagesLink, statisticsLink),
        el(
          "div",
          { className: "cp-toolbar" },
          statusButton,
          themeButton,
          settingsLink,
          accountButton,
        ),
        panel,
        accountMenu,
      ),
      main,
    ),
  );

  enhancePanel(panel, { signal });
  enhanceMenu(accountMenu, {
    signal,
    onSelect: (item) => {
      if (item === signOutItem) {
        void endSession();
      }
    },
  });

  const endSession = async (): Promise<void> => {
    const result = await signOut();
    if (result.ok) {
      leaveTo(routeHref({ screen: "sign-in", next: null }));
      return;
    }
    toast({
      kind: "error",
      title: "Sign out did not finish.",
      body: failureWords(result),
      actions: [{ label: "Retry", run: () => void endSession() }],
    });
  };

  void fetchAccount().then((result) => {
    if (!result.ok) {
      return;
    }
    avatar.textContent = initials(result.data);
    accountButton.setAttribute("aria-label", `Account: ${result.data.username}`);
    signedInAs.textContent = `Signed in as ${result.data.username}`;
  });

  effect(() => {
    const on = opts.resolvedTheme.value === "dark";
    untracked(() => {
      if (themeToggle.on !== on) {
        themeToggle.set(on);
      }
    });
  });

  let wasLive = true;
  effect(() => {
    const c = live.connection.value;
    untracked(() => {
      if (c.kind === "live") {
        statusButton.hidden = true;
        panel.hidePopover();
        if (!wasLive) {
          announce("Connected again.");
        }
        wasLive = true;
        return;
      }
      const words = statusWords(c);
      statusButton.hidden = false;
      statusButton.setAttribute("data-state", c.kind === "offline" ? "offline" : "working");
      statusLabel.textContent = words.label;
      statusDetail.textContent = words.detail ?? "";
      statusDetail.hidden = words.detail === null;
      panelHeading.textContent = words.heading;
      panelFact.replaceChildren(icon(c.kind === "offline" ? "wifi-off" : "refresh-cw"), words.fact);
      panelSentence.textContent = words.sentence;
      tryNow.hidden = c.kind === "offline";
      announce(words.announced);
      wasLive = false;
    });
  });

  let mounted: {
    readonly screen: Route["screen"];
    readonly view: ScreenView;
    readonly stop: AbortController;
  } | null = null;

  // A screen gets a root of its own under the shell's, so a route change
  // that updates it in place never disposes what it built.
  const mount = (route: Route, stop: AbortController): ScreenView =>
    runWithOwner(owner, () =>
      createRoot((dispose) => {
        stop.signal.addEventListener("abort", dispose, { once: true });
        return mountIn(route, stop.signal);
      }),
    ) ?? {};

  const mountIn = (route: Route, stopped: AbortSignal): ScreenView => {
    const ctx: ScreenContext = {
      signal: stopped,
      page: signal,
      live,
      router,
      rail: (label) => {
        const aside = el("aside", { className: "cp-rail", "aria-label": label });
        main.before(aside);
        stopped.addEventListener(
          "abort",
          () => {
            aside.remove();
          },
          { once: true },
        );
        return aside;
      },
    };
    switch (route.screen) {
      case "messages":
        return screens.messages(host, route, ctx);
      case "agent":
        return screens.agent(host, route, ctx);
      case "statistics":
        return screens.statistics(host, route, ctx);
      case "settings":
        return screens.settings(host, route, ctx);
      default:
        host.append(
          el(
            "div",
            { className: "cp-empty" },
            el("h1", null, "This page does not exist"),
            el("p", null, "The page may have moved, or the link was mistyped."),
            el("a", { className: "cp-button", href: "/messages" }, "Go to Messages"),
          ),
        );
        return {};
    }
  };

  const restore = (loc: Located): void => {
    if (loc.cause === "pop") {
      window.scrollTo(0, loc.scroll ?? 0);
      const link =
        loc.focus === null
          ? null
          : main.querySelector<HTMLElement>(`a[href="${CSS.escape(loc.focus)}"]`);
      (link ?? headingOf(main))?.focus({ preventScroll: true });
    } else if (loc.cause === "push") {
      window.scrollTo(0, 0);
      headingOf(main)?.focus({ preventScroll: true });
    }
  };

  const render = (loc: Located): void => {
    const { route } = loc;
    document.title = `${screenTitle(route)} · grelot`;
    setCurrent(messagesLink, currentIn(route, "messages"));
    setCurrent(statisticsLink, currentIn(route, "statistics"));
    setCurrent(settingsLink, currentIn(route, "settings"));
    if (loc.dropped.length > 0) {
      notice.textContent = `This address held ${loc.dropped.join(", ")}, which grelot does not know, so it was left out.`;
      notice.hidden = false;
    } else {
      notice.hidden = true;
      notice.textContent = "";
    }
    if (mounted !== null && mounted.screen === route.screen && mounted.view.update !== undefined) {
      mounted.view.update(route);
      if (loc.cause === "pop") {
        restore(loc);
      }
      return;
    }
    const swap = (): void => {
      mounted?.stop.abort();
      host.replaceChildren();
      const stop = new AbortController();
      mounted = { screen: route.screen, view: mount(route, stop), stop };
      restore(loc);
    };
    if (loc.cause === "load") {
      swap();
    } else {
      void viewTransition(swap);
    }
  };

  effect(() => {
    const loc = router.current.value;
    untracked(() => {
      render(loc);
    });
  });
  signal.addEventListener("abort", () => mounted?.stop.abort(), { once: true });
}

/** The screen's heading, made focusable for a route change. */
function headingOf(main: HTMLElement): HTMLElement | null {
  const h1 = main.querySelector<HTMLElement>("h1");
  if (h1 !== null && !h1.hasAttribute("tabindex")) {
    h1.tabIndex = -1;
  }
  return h1;
}
