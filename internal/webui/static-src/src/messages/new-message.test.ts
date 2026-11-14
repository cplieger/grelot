import { http, HttpResponse } from "msw";
import { describe, expect, it, onTestFinished, vi } from "vitest";
import { userEvent } from "vitest/browser";

import { until } from "../__test-helpers__/until.js";

import type { Agent } from "../wire/types.gen.js";
import {
  answer,
  at,
  button,
  h,
  pageOf,
  rested,
  row,
  rowsShown,
  start,
  useMessagesHarness,
  worker,
} from "./__test-helpers__/harness.js";

useMessagesHarness();

function agent(name: string, over: Partial<Agent> = {}): Agent {
  return {
    name,
    description: "",
    state: "live",
    paths: [],
    follows: [],
    registered_ms: at,
    ...over,
  };
}

function agents(...list: Agent[]): void {
  worker.use(http.get("/api/agents", () => HttpResponse.json({ agents: list })));
}

type Answer = (request: Request) => Response | Promise<Response>;

function posts(...answers: Answer[]): { bodies: unknown[]; keys: (string | null)[] } {
  const seen = { bodies: [] as unknown[], keys: [] as (string | null)[] };
  worker.use(
    http.post("/api/posts", async ({ request }) => {
      seen.keys.push(request.headers.get("Idempotency-Key"));
      seen.bodies.push(await request.json());
      const next = answers.length > 1 ? answers.shift() : answers[0];
      return next === undefined ? HttpResponse.json({}) : next(request);
    }),
  );
  return seen;
}

/** The keys of the POSTs to agent name. An aborted POST from an earlier
 *  test can still reach a later test's handler. */
const keysTo = (seen: ReturnType<typeof posts>, name: string): (string | null)[] =>
  seen.keys.filter((_, i) => JSON.stringify(seen.bodies[i]).includes(`"agent":"${name}"`));

/**
 * A POST answer held until release, then 200. signals holds each POST fetch's
 * signal; answered settles a task after the first one failed or was read.
 */
function heldSend(): {
  answer: Answer;
  release: () => void;
  signals: AbortSignal[];
  answered: () => Promise<void>;
} {
  const held = Promise.withResolvers<undefined>();
  const signals: AbortSignal[] = [];
  const reads: Promise<unknown>[] = [];
  const real = globalThis.fetch.bind(globalThis);
  const spy = vi.spyOn(globalThis, "fetch").mockImplementation((input, init) => {
    const response = real(input, init);
    if (String(input) === "/api/posts" && init?.signal instanceof AbortSignal) {
      signals.push(init.signal);
      reads.push(response.then((r) => r.clone().text()).catch(() => undefined));
    }
    return response;
  });
  onTestFinished(() => {
    spy.mockRestore();
    held.resolve(undefined);
  });
  return {
    answer: async () => {
      await held.promise;
      return HttpResponse.json({ message: 70, thread: 7, delivered: 1 });
    },
    release: () => {
      held.resolve(undefined);
    },
    signals,
    answered: async () => {
      await reads[0];
      await new Promise((done) => setTimeout(done));
    },
  };
}

const dialog = (): HTMLDialogElement => document.getElementById("new-message") as HTMLDialogElement;
const to = (): HTMLInputElement => document.getElementById("new-message-to") as HTMLInputElement;
const subject = (): HTMLInputElement =>
  document.getElementById("new-message-subject") as HTMLInputElement;
const message = (): HTMLTextAreaElement =>
  document.getElementById("new-message-body") as HTMLTextAreaElement;
const options = (): HTMLElement[] => [
  ...dialog().querySelectorAll<HTMLElement>('[role="option"]:not([hidden])'),
];
const errorOf = (field: HTMLElement): string =>
  field.closest(".cp-field")?.querySelector(".cp-field-error:not([hidden])")?.textContent ?? "";
const opener = (): HTMLButtonElement =>
  h.root.querySelector(
    ".cp-heading-row .cp-toolbar button[data-variant='primary']",
  ) as HTMLButtonElement;

async function opened(): Promise<void> {
  opener().focus();
  opener().click();
  await until(() => {
    expect(dialog().open).toBe(true);
  });
}

async function closed(): Promise<void> {
  await until(() => {
    expect(dialog().open).toBe(false);
  });
}

/** Settles once the dialog's next close has reached the screen's own listeners. */
const nextClose = (): Promise<void> =>
  new Promise((done) => {
    dialog().addEventListener(
      "close",
      () => {
        done();
      },
      { once: true },
    );
  });

async function filled(name: string): Promise<void> {
  await userEvent.fill(to(), `@${name}`);
  await userEvent.fill(subject(), "Review");
  await userEvent.fill(message(), "When you are back.");
}

/** Checks that no toast says the post reached name and that the screen
 *  stayed on the list. A name per test, since toasts outlive a test. */
function nothingSent(name: string): void {
  expect(document.body.textContent).not.toContain(`Sent to @${name}.`);
  expect(location.pathname).toBe("/messages");
}

/** Without overlay support Chromium plays a dialog's exit in script, as
 *  Firefox and WebKit do. */
function scriptedExit(): void {
  const supports = CSS.supports.bind(CSS) as (...args: string[]) => boolean;
  const spy = vi
    .spyOn(CSS, "supports")
    .mockImplementation((...args: string[]) => args[0] !== "overlay" && supports(...args));
  onTestFinished(() => {
    spy.mockRestore();
  });
}

/** The dialog's scripted exit, once it starts, paused until played. */
async function pausedExit(): Promise<Animation[]> {
  await until(() => {
    expect(dialog().getAnimations().length).toBeGreaterThan(0);
  });
  const exits = dialog().getAnimations();
  for (const a of exits) {
    a.pause();
  }
  return exits;
}

describe("the New message dialog", () => {
  it("opens on To, offering Announcements first and then the agents, dormant ones marked", async () => {
    agents(agent("alpha"), agent("bravo", { state: "dormant" }), agent("charlie"));
    start("/messages");
    await rowsShown([]);
    expect(opener().textContent).toBe("New message");
    await opened();
    expect(document.activeElement).toBe(to());
    expect(dialog().getAttribute("aria-labelledby")).toBe("new-message-title");
    expect(document.getElementById("new-message-title")?.textContent).toBe("New message");
    await until(() => {
      expect(options().map((o) => o.textContent)).toEqual([
        "Announcements",
        "@alpha",
        "@bravodormant",
        "@charlie",
      ]);
    });
    expect(options()[2]?.querySelector("small")?.textContent).toBe("dormant");
    expect(dialog().querySelector(".cp-listbox:popover-open")).toBeNull();

    await userEvent.keyboard("br");
    expect(options().map((o) => o.textContent)).toEqual(["@bravodormant"]);
    await userEvent.keyboard("{Enter}");
    expect(to().value).toBe("@bravo");
  });

  it("sends to the chosen agent, then opens the conversation with focus on its heading and says so", async () => {
    agents(agent("alpha"), agent("bravo", { state: "dormant" }));
    answer(
      pageOf([]),
      pageOf([row(7, { kind: "direct", title: "@bravo and @user", agents: ["bravo", "user"] })]),
    );
    const sent = posts(() => HttpResponse.json({ message: 70, thread: 7, delivered: 1 }));
    start("/messages");
    await rowsShown([]);
    await opened();
    await userEvent.fill(to(), "@bravo");
    await userEvent.fill(subject(), "Review");
    await userEvent.fill(message(), "When you are back.");
    button("Send", dialog()).click();

    await closed();
    expect(sent.bodies).toEqual([
      { to: { agent: "bravo" }, subject: "Review", body: "When you are back." },
    ]);
    expect(sent.keys[0]).toMatch(/^"[0-9a-f]{32}"$/);
    await until(() => {
      expect(location.pathname).toBe("/messages/7");
    });
    await until(() => {
      expect(document.activeElement).toBe(h.root.querySelector(".cp-detail h2"));
    });
    await until(() => {
      expect(document.body.textContent).toContain("Sent to @bravo.");
    });
    expect(document.body.textContent).toContain(
      "@bravo is dormant and reads it once it registers again.",
    );
  });

  it("sends Announcements to announce", async () => {
    const sent = posts(() => HttpResponse.json({ message: 30, thread: 3, delivered: 4 }));
    start("/messages");
    await rowsShown([]);
    await opened();
    await userEvent.fill(to(), "announcements");
    await userEvent.fill(subject(), "Deploy");
    button("Send", dialog()).click();
    await closed();
    expect(sent.bodies).toEqual([{ to: { announce: true }, subject: "Deploy", body: "" }]);
    await until(() => {
      expect(document.body.textContent).toContain("Sent to Announcements.");
    });
  });

  it("says what to fix before sending: one error on its field, two in the summary", async () => {
    const sent = posts();
    start("/messages");
    await rowsShown([]);
    await opened();
    await userEvent.fill(subject(), "Hello");
    button("Send", dialog()).click();
    await until(() => {
      expect(errorOf(to())).toBe("Choose who gets it: an agent, or Announcements.");
    });
    expect(document.activeElement).toBe(to());

    await userEvent.fill(to(), "@user");
    await userEvent.fill(subject(), "");
    button("Send", dialog()).click();
    await until(() => {
      expect(errorOf(to())).toBe("Choose an agent: @user is you.");
    });
    expect(errorOf(subject())).toBe("Enter a subject.");
    const summary = dialog().querySelector('.cp-notice[data-tone="danger"]') as HTMLElement;
    expect(summary.textContent).toContain("2 fields need a fix before this sends");
    expect(document.activeElement).toBe(summary);
    expect(sent.bodies).toEqual([]);
    expect(dialog().open).toBe(true);
  });

  it("shows the server's refusal under To, and sending the same post again reuses its key", async () => {
    const sent = posts(
      () =>
        HttpResponse.json(
          { error: "no agent is called @nobody", code: "not_found" },
          { status: 404 },
        ),
      () =>
        HttpResponse.json(
          { error: "no agent is called @nobody", code: "not_found" },
          { status: 404 },
        ),
      () => HttpResponse.json({ message: 50, thread: 5, delivered: 1 }),
    );
    start("/messages");
    await rowsShown([]);
    await opened();
    await userEvent.fill(to(), "@nobody");
    await userEvent.fill(subject(), "Hello");
    button("Send", dialog()).click();
    await until(() => {
      expect(errorOf(to())).toBe("no agent is called @nobody");
    });
    expect(subject().value).toBe("Hello");
    (await rested("Send", dialog())).click();
    await until(() => {
      expect(sent.keys.length).toBe(2);
    });
    expect(sent.keys[1]).toBe(sent.keys[0]);

    await userEvent.fill(to(), "@alpha");
    (await rested("Send", dialog())).click();
    await closed();
    expect(sent.keys[2]).not.toBe(sent.keys[0]);
  });

  it("closes on Cancel with what was typed, focus back on its opener, and opens empty again", async () => {
    start("/messages");
    await rowsShown([]);
    await opened();
    await userEvent.fill(subject(), "Half");
    button("Cancel", dialog()).click();
    await closed();
    expect(document.querySelectorAll("dialog[open]").length).toBe(0);
    expect(document.activeElement).toBe(opener());

    await opened();
    expect(subject().value).toBe("");
  });

  it("aborts a send on Cancel, and its answer toasts and navigates on no later close", async () => {
    const held = heldSend();
    posts(held.answer);
    start("/messages");
    await rowsShown([]);
    await opened();
    await filled("charlie");
    button("Send", dialog()).click();
    await until(() => {
      expect(held.signals.length).toBe(1);
    });
    const cancelled = nextClose();
    button("Cancel", dialog()).click();
    await cancelled;
    held.release();
    await held.answered();
    nothingSent("charlie");

    await opened();
    const again = nextClose();
    button("Cancel", dialog()).click();
    await again;
    nothingSent("charlie");
    expect(held.signals.length).toBe(1);
  });

  it("aborts the request in flight on Cancel", async () => {
    const held = heldSend();
    posts(held.answer);
    start("/messages");
    await rowsShown([]);
    await opened();
    await filled("delta");
    button("Send", dialog()).click();
    await until(() => {
      expect(held.signals.length).toBe(1);
    });
    expect(held.signals[0]?.aborted).toBe(false);
    button("Cancel", dialog()).click();
    await until(() => {
      expect(held.signals[0]?.aborted).toBe(true);
    });
  });

  it("sends the same post again after a Cancel with the cancelled send's key", async () => {
    const held = heldSend();
    const sent = posts(held.answer, () =>
      HttpResponse.json({ message: 70, thread: 7, delivered: 1 }),
    );
    start("/messages");
    await rowsShown([]);
    await opened();
    await filled("echo");
    button("Send", dialog()).click();
    await until(() => {
      expect(held.signals.length).toBe(1);
    });
    button("Cancel", dialog()).click();
    await closed();
    held.release();
    await held.answered();
    await rested("Send", dialog());

    await opened();
    await filled("echo");
    button("Send", dialog()).click();
    await closed();
    const echoes = keysTo(sent, "echo");
    expect(echoes.length).toBe(2);
    expect(echoes[1]).toBe(echoes[0]);
    await until(() => {
      expect(document.body.textContent).toContain("Sent to @echo.");
    });
  });

  it.each([
    {
      how: "Escape",
      name: "foxtrot",
      leave: async () => {
        message().focus();
        await userEvent.keyboard("{Escape}");
      },
    },
    {
      how: "Cancel",
      name: "golf",
      leave: () => {
        button("Cancel", dialog()).click();
        return Promise.resolve();
      },
    },
  ])(
    "says nothing of a send whose answer comes during $how's exit, takes no send then, and keeps its key",
    async ({ name, leave }) => {
      scriptedExit();
      const held = heldSend();
      const sent = posts(held.answer, () =>
        HttpResponse.json({ message: 70, thread: 7, delivered: 1 }),
      );
      start("/messages");
      await rowsShown([]);
      await opened();
      await filled(name);
      button("Send", dialog()).click();
      await until(() => {
        expect(held.signals.length).toBe(1);
      });
      await leave();
      const exits = await pausedExit();
      held.release();
      await rested("Send", dialog());
      expect(dialog().open).toBe(true);
      await userEvent.fill(message(), "Something else.");
      button("Send", dialog()).click();
      await new Promise((done) => setTimeout(done));
      const left = nextClose();
      for (const a of exits) {
        a.play();
      }
      await left;
      nothingSent(name);
      expect(held.signals.length).toBe(1);

      await opened();
      await filled(name);
      button("Send", dialog()).click();
      await closed();
      const keys = keysTo(sent, name);
      expect(keys.length).toBe(2);
      expect(keys[1]).toBe(keys[0]);
    },
  );

  it("takes no second send during a sent post's exit, and says it was sent once", async () => {
    scriptedExit();
    const fetched = heldSend();
    posts(() => HttpResponse.json({ message: 70, thread: 7, delivered: 1 }));
    start("/messages");
    await rowsShown([]);
    await opened();
    await filled("hotel");
    button("Send", dialog()).click();
    const exits = await pausedExit();
    await rested("Send", dialog());
    await userEvent.fill(message(), "Something else.");
    button("Send", dialog()).click();
    await new Promise((done) => setTimeout(done));
    const left = nextClose();
    for (const a of exits) {
      a.play();
    }
    await left;
    await until(() => {
      expect(document.body.textContent).toContain("Sent to @hotel.");
    });
    expect(fetched.signals.length).toBe(1);
    const toasts = [...document.querySelectorAll(".cp-toast")].filter((t) =>
      t.textContent.includes("Sent to @hotel."),
    );
    expect(toasts.length).toBe(1);
  });

  it("opens from the empty list's New message", async () => {
    start("/messages");
    await rowsShown([]);
    await until(() => {
      expect(h.root.querySelector(".cp-empty")?.textContent).toContain(
        "No conversations in the last 72 hours.",
      );
    });
    button("New message", h.root.querySelector(".cp-empty") as HTMLElement).click();
    await until(() => {
      expect(dialog().open).toBe(true);
    });
  });
});
