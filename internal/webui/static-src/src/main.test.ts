import { expect, it } from "vitest";

it("keeps the library's tooltip and live regions in the page once a screen is drawn", async () => {
  history.replaceState(null, "", "/sign-in");
  await import("./main.js");

  expect(document.querySelector(".cp-gate")).not.toBeNull();
  expect(document.getElementById("cp-tooltip")?.isConnected).toBe(true);
  expect(document.body.querySelector(":scope > [data-cp-live-regions]")).not.toBeNull();
  expect(document.body.firstElementChild?.querySelector(".cp-gate")).not.toBeNull();
});
