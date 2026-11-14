import { el } from "@cplieger/reactive/dom";

import type { Route } from "./router.js";
import { screenTitle } from "./shell.js";

type GateRoute = Extract<Route, { readonly screen: "sign-in" | "setup" | "recover" }>;

const headings: Readonly<Record<GateRoute["screen"], string>> = {
  "sign-in": "Sign in",
  setup: "Set up grelot",
  recover: "Set a new password",
};

/**
 * Draws a gate page, outside the frame. Until @cplieger/ui ships the shared
 * account screens, each says so in place of its form.
 */
export function renderGate(root: HTMLElement, route: GateRoute): void {
  document.title = route.screen === "setup" ? screenTitle(route) : `${screenTitle(route)} · grelot`;
  root.replaceChildren(
    el(
      "main",
      { className: "cp-gate" },
      el(
        "a",
        { className: "cp-wordmark", href: "/messages", "aria-label": "grelot" },
        el("span", { className: "cp-mark", "aria-hidden": "true" }, "g"),
        el("span", null, "grelot"),
      ),
      el(
        "div",
        { className: "cp-panel" },
        el("h1", null, headings[route.screen]),
        el("div", { className: "cp-empty" }, el("p", null, "Sign-in is not wired yet.")),
      ),
    ),
  );
}
