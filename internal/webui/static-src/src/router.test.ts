import { afterEach, describe, expect, it } from "vitest";

import { until } from "./__test-helpers__/until.js";
import { parseRoute, routeHref, signInHref, startRouter, type Route } from "./router.js";

// The addresses shape.md gives each screen, with the state each opens.
const addresses: readonly (readonly [string, Route])[] = [
  ["/messages", { screen: "messages", show: "all", conversation: null }],
  ["/messages?show=threads", { screen: "messages", show: "threads", conversation: null }],
  ["/messages/7?show=direct", { screen: "messages", show: "direct", conversation: 7 }],
  ["/agents/kiro-2", { screen: "agent", name: "kiro-2" }],
  ["/statistics", { screen: "statistics", range: "24h", problems: null }],
  ["/statistics?range=6h&problems=push", { screen: "statistics", range: "6h", problems: "push" }],
  ["/settings", { screen: "settings", section: null }],
  ["/settings/sign-in", { screen: "settings", section: "sign-in" }],
  [
    "/sign-in?next=%2Fmessages%2F7%3Fshow%3Ddirect",
    { screen: "sign-in", next: "/messages/7?show=direct" },
  ],
  ["/setup", { screen: "setup" }],
  ["/recover", { screen: "recover" }],
];

function parse(href: string): ReturnType<typeof parseRoute> {
  const url = new URL(href, "https://grelot.test");
  return parseRoute(url.pathname, url.search);
}

describe("parseRoute and routeHref", () => {
  it.each(addresses)("reads %s and writes it back", (href, route) => {
    expect(parse(href)).toEqual({ route, dropped: [] });
    expect(routeHref(route)).toBe(href);
  });

  it("opens Messages at the root, with the defaults omitted from the address", () => {
    const { route } = parse("/?show=all");
    expect(route).toEqual({ screen: "messages", show: "all", conversation: null });
    expect(routeHref(route)).toBe("/messages");
    expect(routeHref({ screen: "statistics", range: "24h", problems: null })).toBe("/statistics");
  });

  it.each([
    [
      "/messages?show=bogus",
      { screen: "messages", show: "all", conversation: null },
      ["show=bogus"],
    ],
    [
      "/messages?show=threads&show=direct",
      { screen: "messages", show: "threads", conversation: null },
      ["show=direct"],
    ],
    [
      "/messages?utm=x&flag",
      { screen: "messages", show: "all", conversation: null },
      ["utm=x", "flag"],
    ],
    [
      "/messages/abc",
      { screen: "messages", show: "all", conversation: null },
      ["conversation abc"],
    ],
    ["/messages/0", { screen: "messages", show: "all", conversation: null }, ["conversation 0"]],
    [
      "/messages/99999999999999999",
      { screen: "messages", show: "all", conversation: null },
      ["conversation 99999999999999999"],
    ],
    [
      "/agents/Not_A_Name",
      { screen: "messages", show: "all", conversation: null },
      ["agent Not_A_Name"],
    ],
    [
      "/statistics?range=1h&problems=push",
      { screen: "statistics", range: "24h", problems: "push" },
      ["range=1h"],
    ],
    ["/settings/machines", { screen: "settings", section: null }, ["settings machines"]],
    [
      "/sign-in?next=//evil.example/x",
      { screen: "sign-in", next: null },
      ["next=//evil.example/x"],
    ],
    [
      "/sign-in?next=https://evil.example",
      { screen: "sign-in", next: null },
      ["next=https://evil.example"],
    ],
    ["/sign-in?next=/\\evil.example", { screen: "sign-in", next: null }, ["next=/\\evil.example"]],
    [
      "/sign-in?next=%2F%09%2Fevil.example",
      { screen: "sign-in", next: null },
      ["next=/\t/evil.example"],
    ],
    [
      "/sign-in?next=%2F%0A%2Fevil.example",
      { screen: "sign-in", next: null },
      ["next=/\n/evil.example"],
    ],
    [
      "/sign-in?next=%2F%0D%5Cevil.example",
      { screen: "sign-in", next: null },
      ["next=/\r\\evil.example"],
    ],
  ] as const)(
    "opens the nearest valid view for %s and names what it dropped",
    (href, route, dropped) => {
      expect(parse(href)).toEqual({ route, dropped });
    },
  );

  it.each(["/nope", "/messages/7/extra", "/agents", "/statistics/x", "/sign-in/x", "/%E0%A4%A"])(
    "answers %s with not-found",
    (path) => {
      const { route } = parse(path);
      expect(route).toEqual({ screen: "not-found", path });
    },
  );

  it("encodes an agent's name in its address", () => {
    expect(routeHref({ screen: "agent", name: "a-b" })).toBe("/agents/a-b");
    expect(parse(routeHref({ screen: "agent", name: "a-b" })).route).toEqual({
      screen: "agent",
      name: "a-b",
    });
  });
});

describe("signInHref", () => {
  it("comes back to the address a refused request was made from", () => {
    expect(signInHref("/messages/7", "?show=direct")).toBe(
      "/sign-in?next=%2Fmessages%2F7%3Fshow%3Ddirect",
    );
    expect(parse(signInHref("/messages/7", "?show=direct") ?? "").route).toEqual({
      screen: "sign-in",
      next: "/messages/7?show=direct",
    });
  });

  it("never sends a gate page to sign-in, so a refusal there cannot loop", () => {
    expect(signInHref("/sign-in", "?next=%2Fmessages")).toBeNull();
    expect(signInHref("/setup", "")).toBeNull();
    expect(signInHref("/recover", "")).toBeNull();
  });

  it("drops an address no screen answers rather than coming back to it", () => {
    expect(signInHref("/nope", "")).toBe("/sign-in");
  });
});

describe("startRouter", () => {
  const originalHref = location.href;
  let stop = new AbortController();
  let filler: HTMLElement | null = null;

  afterEach(() => {
    stop.abort();
    stop = new AbortController();
    filler?.remove();
    filler = null;
    document.body.replaceChildren();
    window.scrollTo(0, 0);
    history.replaceState(null, "", originalHref);
  });

  function popped(): Promise<void> {
    return new Promise((resolve) => {
      window.addEventListener("popstate", () => resolve(), { once: true });
    });
  }

  function link(href: string): HTMLAnchorElement {
    const a = document.createElement("a");
    a.href = href;
    a.textContent = href;
    document.body.append(a);
    return a;
  }

  it("replaces an address holding unknown values with the canonical one and keeps what it dropped", () => {
    history.replaceState(null, "", "/messages?show=bogus");
    const router = startRouter(stop.signal);
    expect(location.pathname + location.search).toBe("/messages");
    expect(router.current.value).toMatchObject({
      route: { screen: "messages", show: "all" },
      dropped: ["show=bogus"],
      cause: "load",
    });
  });

  it("takes over a link to one of its screens as a new history entry", () => {
    history.replaceState(null, "", "/messages");
    const router = startRouter(stop.signal);
    const before = history.length;
    link("/agents/kiro").click();
    expect(location.pathname).toBe("/agents/kiro");
    expect(history.length).toBe(before + 1);
    expect(router.current.value).toMatchObject({
      route: { screen: "agent", name: "kiro" },
      cause: "push",
    });
  });

  it("leaves a modified click, a gate page and a new-tab link to the browser", () => {
    history.replaceState(null, "", "/messages");
    const router = startRouter(stop.signal);
    const passed: boolean[] = [];
    // Runs after the router's document listener; it stops the browser's own
    // navigation, which would leave the test page.
    window.addEventListener(
      "click",
      (e) => {
        passed.push(!e.defaultPrevented);
        e.preventDefault();
      },
      { signal: stop.signal },
    );
    link("/statistics").dispatchEvent(
      new MouseEvent("click", { bubbles: true, cancelable: true, ctrlKey: true }),
    );
    link("/sign-in").click();
    const blank = link("/statistics");
    blank.target = "_blank";
    blank.click();
    expect(passed).toEqual([true, true, true]);
    expect(router.current.value.route.screen).toBe("messages");
  });

  it("does not stack an entry for a link to the address already open", () => {
    history.replaceState(null, "", "/statistics");
    startRouter(stop.signal);
    const before = history.length;
    link("/statistics").click();
    expect(history.length).toBe(before);
    expect(location.pathname).toBe("/statistics");
  });

  it("replaces the entry when asked, so Back skips the old address", () => {
    history.replaceState(null, "", "/messages");
    const router = startRouter(stop.signal);
    const before = history.length;
    router.go({ screen: "messages", show: "direct", conversation: null }, { replace: true });
    expect(location.search).toBe("?show=direct");
    expect(history.length).toBe(before);
    expect(router.current.value.cause).toBe("replace");
  });

  it("gives Back the scroll and the focused link the reader left", async () => {
    filler = document.createElement("div");
    filler.style.blockSize = "4000px";
    document.body.append(filler);
    history.replaceState(null, "", "/messages");
    const router = startRouter(stop.signal);
    window.scrollTo(0, 600);
    const agent = link("/agents/kiro");
    agent.focus({ preventScroll: true });
    router.go({ screen: "statistics", range: "24h", problems: null });
    expect(router.current.value).toMatchObject({ scroll: null, focus: null, cause: "push" });

    const back = popped();
    history.back();
    await back;
    expect(router.current.value).toMatchObject({
      route: { screen: "messages", show: "all", conversation: null },
      cause: "pop",
      scroll: 600,
      focus: "/agents/kiro",
    });
  });

  it("asks a hold before a move it holds, and moves only on yes", async () => {
    history.replaceState(null, "", "/settings");
    const router = startRouter(stop.signal);
    const answers = [false, true];
    const asked: string[] = [];
    router.hold(
      {
        holds: (next) => next.screen !== "settings",
        ask: () => {
          asked.push(location.pathname);
          return Promise.resolve(answers.shift() ?? false);
        },
      },
      stop.signal,
    );

    router.go({ screen: "settings", section: "sign-in" });
    expect(location.pathname).toBe("/settings/sign-in");

    link("/statistics").click();
    await new Promise((settle) => setTimeout(settle, 0));
    expect(location.pathname).toBe("/settings/sign-in");

    link("/statistics").click();
    await until(() => {
      expect(location.pathname).toBe("/statistics");
    });
    expect(asked).toEqual(["/settings/sign-in", "/settings/sign-in"]);
  });

  it("puts the address back when Back is declined, and goes back on yes", async () => {
    history.replaceState(null, "", "/messages");
    const router = startRouter(stop.signal);
    router.go({ screen: "settings", section: null });
    const answers = [false, true];
    router.hold(
      { holds: () => true, ask: () => Promise.resolve(answers.shift() ?? false) },
      stop.signal,
    );

    const declined = popped();
    history.back();
    await declined;
    expect(location.pathname).toBe("/settings");
    expect(router.current.value.route.screen).toBe("settings");

    history.back();
    await until(() => {
      expect(router.current.value.route.screen).toBe("messages");
    });
    expect(location.pathname).toBe("/messages");
  });

  it("lets a hold go once its lifetime ends", () => {
    history.replaceState(null, "", "/settings");
    const router = startRouter(stop.signal);
    const lifetime = new AbortController();
    router.hold({ holds: () => true, ask: () => Promise.resolve(false) }, lifetime.signal);
    lifetime.abort();

    router.go({ screen: "statistics", range: "24h", problems: null });

    expect(location.pathname).toBe("/statistics");
  });

  it("stops following links once its signal aborts", () => {
    history.replaceState(null, "", "/messages");
    const router = startRouter(stop.signal);
    stop.abort();
    window.addEventListener("click", (e) => e.preventDefault(), { once: true });
    link("/statistics").click();
    expect(router.current.value.route.screen).toBe("messages");
  });
});
