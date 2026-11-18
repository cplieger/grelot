import { afterEach, describe, expect, it } from "vitest";

import { renderGate } from "./gate.js";

describe("renderGate", () => {
  const root = document.createElement("div");
  document.body.append(root);

  afterEach(() => {
    root.replaceChildren();
  });

  it.each([
    [{ screen: "sign-in", next: null }, "Sign in", "Sign in · grelot"],
    [{ screen: "setup" }, "Set up grelot", "Set up grelot"],
    [{ screen: "recover" }, "Set a new password", "Recover your account · grelot"],
  ] as const)(
    "draws %o outside the frame, saying sign-in is not wired yet",
    (route, heading, title) => {
      renderGate(root, route);
      expect(root.querySelector(".cp-gate h1")?.textContent).toBe(heading);
      expect(document.title).toBe(title);
      expect(root.querySelector(".cp-gate .cp-empty")?.textContent).toBe(
        "Sign-in is not wired yet.",
      );
      expect(root.querySelector(".cp-app")).toBeNull();
    },
  );
});
