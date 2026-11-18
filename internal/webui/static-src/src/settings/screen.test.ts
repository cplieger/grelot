import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { userEvent } from "vitest/browser";

import { until } from "../__test-helpers__/until.js";
import {
  button,
  h,
  hour,
  rested,
  settingsOf,
  start,
  useMessagesHarness,
  worker,
} from "../messages/__test-helpers__/harness.js";
import type { Settings, SettingsRequest } from "../wire/types.gen.js";
import { mountSettings } from "./screen.js";

useMessagesHarness();

function settings(over: Partial<Settings> = {}): Settings {
  return settingsOf({ revision: 3, ...over });
}

/** GET /api/settings answers s; each PUT is recorded and answers the next of puts, the last
 * repeating, or with none the body saved at the next revision. */
function serve(s: Settings, ...puts: (() => Response)[]): SettingsRequest[] {
  const sent: SettingsRequest[] = [];
  worker.use(
    http.get("/api/settings", () => HttpResponse.json(s)),
    http.put("/api/settings", async ({ request }) => {
      const body = (await request.json()) as SettingsRequest;
      sent.push(body);
      const next = puts.length > 1 ? puts.shift() : puts[0];
      return next === undefined
        ? HttpResponse.json({
            ...s,
            revision: body.revision + 1,
            retention_ms: body.retention_ms,
            methods: body.methods,
          })
        : next();
    }),
  );
  return sent;
}

const refusal =
  (status: number, code: string, error = "refused") =>
  () =>
    HttpResponse.json({ code, error }, { status });

const field = (id: string): HTMLInputElement => {
  const f = h.root.querySelector<HTMLInputElement>(`#${id}`);
  if (f === null) {
    throw new Error(`no field ${id}`);
  }
  return f;
};
const retention = (): HTMLInputElement => field("settings-retention");
const box = (key: "password" | "passkeys" | "oidc"): HTMLInputElement => field(`settings-${key}`);
const savebar = (): HTMLElement | null => h.root.querySelector(".cp-savebar");
const navLink = (words: string): HTMLAnchorElement => {
  const a = [...h.root.querySelectorAll<HTMLAnchorElement>(".cp-settings-nav a")].find((x) =>
    x.textContent.startsWith(words),
  );
  if (a === undefined) {
    throw new Error(`no category ${words}`);
  }
  return a;
};
const errorOf = (f: HTMLInputElement): string | null =>
  f.getAttribute("aria-invalid") === "true"
    ? (h.root.querySelector(`#${f.id}-error`)?.textContent ?? null)
    : null;
const retries = (): HTMLButtonElement[] =>
  [...h.root.querySelectorAll<HTMLButtonElement>("button")].filter(
    (b) => b.textContent === "Retry",
  );
const question = (): HTMLDialogElement | null =>
  document.querySelector<HTMLDialogElement>('dialog[data-variant="confirm"][open]');

async function open(href = "/settings"): Promise<void> {
  start(href, { settings: mountSettings });
  await until(() => {
    expect(retention().value).not.toBe("");
  });
}

async function type(f: HTMLInputElement, value: string): Promise<void> {
  await userEvent.clear(f);
  await userEvent.type(f, value);
}

describe("the Settings screen", () => {
  it("opens History with the stored retention and the sign-in methods as stored", async () => {
    serve(
      settings({
        retention_ms: 96 * hour,
        methods: { password: true, passkeys: false, oidc: true },
      }),
    );
    await open();

    expect(h.root.querySelector("h1")?.textContent).toBe("Settings");
    expect(navLink("History").getAttribute("aria-current")).toBe("page");
    expect(navLink("Sign-in").hasAttribute("aria-current")).toBe(false);
    expect(retention().value).toBe("96");
    expect(retention().labels?.[0]?.textContent).toBe("Keep history for (hours)");
    expect(retention().min).toBe("1");
    expect(retention().max).toBe("2160");
    expect(savebar()?.hidden).toBe(true);
    expect(retention().closest<HTMLElement>("[hidden]")).toBeNull();
    expect(box("password").closest<HTMLElement>("[hidden]")).not.toBeNull();

    expect([box("password").checked, box("passkeys").checked, box("oidc").checked]).toEqual([
      true,
      false,
      true,
    ]);
    expect(box("oidc").disabled).toBe(true);
    expect(h.root.querySelector("#settings-oidc-help")?.textContent).toBe(
      "Set OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET to offer it.",
    );
  });

  it("offers single sign-on once the server has a provider", async () => {
    serve(settings({ oidc_configured: true }));
    await open("/settings/sign-in");

    expect(box("oidc").disabled).toBe(false);
    expect(h.root.querySelector("#settings-oidc-help")?.textContent).not.toMatch(/OIDC_ISSUER/);
  });

  it("opens the category its address names, and a category link keeps the unsaved changes", async () => {
    serve(settings());
    await open("/settings/sign-in");
    expect(navLink("Sign-in").getAttribute("aria-current")).toBe("page");
    expect(box("password").closest<HTMLElement>("[hidden]")).toBeNull();
    expect(retention().closest<HTMLElement>("[hidden]")).not.toBeNull();

    await userEvent.click(box("passkeys"));
    await userEvent.click(navLink("History"));

    expect(location.pathname).toBe("/settings/history");
    expect(question()).toBeNull();
    expect(box("passkeys").checked).toBe(false);
    expect(savebar()?.textContent).toMatch(/^1 unsaved change/);
  });

  it("counts each unsaved change in the bar and marks its category", async () => {
    serve(settings());
    await open();

    await type(retention(), "168");
    expect(savebar()?.hidden).toBe(false);
    expect(savebar()?.textContent).toMatch(/^1 unsaved change(?!s)/);
    expect(navLink("History").textContent).toBe("History, unsaved changes");
    expect(navLink("Sign-in").textContent).toBe("Sign-in");

    box("passkeys").click();
    expect(savebar()?.textContent).toMatch(/^2 unsaved changes/);
    expect(navLink("Sign-in").textContent).toBe("Sign-in, unsaved changes");

    box("password").click();
    expect(savebar()?.textContent).toMatch(/^3 unsaved changes/);
    box("password").click();

    await type(retention(), "72");
    expect(savebar()?.textContent).toMatch(/^1 unsaved change(?!s)/);
    expect(navLink("History").textContent).toBe("History");
  });

  it("saves the changes, says so, and leaves nothing unsaved", async () => {
    const sent = serve(settings());
    await open();

    await type(retention(), "168");
    box("passkeys").click();
    await userEvent.click(button("Save changes"));

    await until(() => {
      expect(savebar()?.hidden).toBe(true);
    });
    expect(sent).toEqual([
      {
        revision: 3,
        retention_ms: 168 * hour,
        methods: { password: true, passkeys: false, oidc: false },
      },
    ]);
    expect(document.body.textContent).toContain("Settings saved.");
    expect(navLink("History").textContent).toBe("History");
    expect(document.activeElement?.textContent).toBe("History");
  });

  it("saves the next change over the revision the last save answered", async () => {
    const sent = serve(settings());
    await open();

    await type(retention(), "168");
    await userEvent.click(button("Save changes"));
    await until(() => {
      expect(savebar()?.hidden).toBe(true);
    });
    await type(retention(), "24");
    await userEvent.click(await rested("Save changes"));

    await until(() => {
      expect(sent).toHaveLength(2);
    });
    expect(sent.map((b) => b.revision)).toEqual([3, 4]);
  });

  it("says a stale save as any refused save, with no Retry, and keeps the changes", async () => {
    const sent = serve(
      settings(),
      refusal(409, "stale_revision", "the settings changed since they were read: read them again"),
    );
    await open();

    await type(retention(), "168");
    await userEvent.click(button("Save changes"));

    await until(() => {
      expect(h.root.querySelector("form .cp-notice")?.textContent).toBe(
        "Settings were not saved. the settings changed since they were read: read them again",
      );
    });
    expect(retries()).toEqual([]);
    expect(retention().value).toBe("168");
    expect(savebar()?.textContent).toMatch(/^1 unsaved change/);
    expect(sent.map((b) => b.revision)).toEqual([3]);
  });

  it("sends Enter in the retention field as Save", async () => {
    const sent = serve(settings());
    await open();

    await type(retention(), "24{Enter}");

    await until(() => {
      expect(sent).toHaveLength(1);
    });
    expect(sent[0]?.retention_ms).toBe(24 * hour);
  });

  it("puts every stored value back on Discard", async () => {
    const sent = serve(settings());
    await open();

    await type(retention(), "500");
    box("password").click();
    await userEvent.click(button("Discard"));

    expect(retention().value).toBe("72");
    expect(box("password").checked).toBe(true);
    expect(savebar()?.hidden).toBe(true);
    expect(sent).toEqual([]);
    expect(document.activeElement?.textContent).toBe("History");
  });

  it.each(["0", "2161", "1.5", ""])(
    "refuses %j hours on Save with the bounds, sends nothing, and clears the error once valid",
    async (typed) => {
      const sent = serve(settings());
      await open();

      await userEvent.clear(retention());
      await userEvent.type(retention(), `${typed}{Enter}`);

      expect(errorOf(retention())).toBe("Enter a whole number of hours from 1 to 2160.");
      expect(document.activeElement).toBe(retention());
      expect(sent).toEqual([]);

      await type(retention(), "48");
      expect(errorOf(retention())).toBeNull();
    },
  );

  it("shows the bounds when the field is left holding a value outside them", async () => {
    serve(settings());
    await open();

    await type(retention(), "9000");
    await userEvent.tab();

    expect(errorOf(retention())).toBe("Enter a whole number of hours from 1 to 2160.");
  });

  it("opens History to show a retention the server refused", async () => {
    serve(settings(), refusal(400, "invalid"));
    await open("/settings/sign-in");
    navLink("History").click();
    await type(retention(), "100");
    navLink("Sign-in").click();

    await userEvent.click(button("Save changes"));

    await until(() => {
      expect(errorOf(retention())).toBe("Enter a whole number of hours from 1 to 2160.");
    });
    expect(location.pathname).toBe("/settings/history");
    expect(document.activeElement).toBe(retention());
    expect(retries()).toEqual([]);
  });

  it("names the method turned off when the change would lock the account out", async () => {
    serve(settings(), refusal(409, "would_lock_out"));
    await open("/settings/sign-in");

    box("passkeys").click();
    await userEvent.click(button("Save changes"));

    await until(() => {
      expect(errorOf(box("passkeys"))).toBe(
        "You would have no way to sign in. Turn on another method first.",
      );
    });
    expect(errorOf(box("password"))).toBeNull();
    expect(document.activeElement).toBe(box("passkeys"));
    expect(box("passkeys").checked).toBe(false);
    expect(savebar()?.hidden).toBe(false);

    box("password").click();
    expect(errorOf(box("passkeys"))).toBeNull();
  });

  it("asks for a recent sign-in when the server wants one, keeping the changes", async () => {
    serve(settings(), refusal(403, "recent_auth_required"));
    await open("/settings/sign-in");

    box("password").click();
    await userEvent.click(button("Save changes"));

    await until(() => {
      expect(h.root.querySelector("form .cp-notice")?.textContent).toBe(
        "Sign in again to change sign-in methods.",
      );
    });
    expect(box("password").checked).toBe(false);
    expect(savebar()?.textContent).toMatch(/^1 unsaved change/);
  });

  it("says a save that ran out of time in the server's words and saves on Retry", async () => {
    const sent = serve(
      settings(),
      () =>
        HttpResponse.json(
          { code: "timeout", error: "The request ran out of time.", retry_after_ms: 1000 },
          { status: 503 },
        ),
      () => HttpResponse.json(settings({ revision: 4, retention_ms: 12 * hour })),
    );
    await open();

    await type(retention(), "12");
    await userEvent.click(button("Save changes"));

    const notice = await until(() => {
      const n = h.root.querySelector<HTMLElement>("form .cp-notice");
      expect(n?.textContent).toBe("Settings were not saved. The request ran out of time.Retry");
      return n;
    });
    await userEvent.click(button("Retry", notice ?? undefined));

    await until(() => {
      expect(savebar()?.hidden).toBe(true);
    });
    expect(sent).toHaveLength(2);
    expect(sent[1]).toEqual(sent[0]);
    expect(h.root.querySelector("form .cp-notice")).toBeNull();
  });

  it("offers Retry on a save that got no answer", async () => {
    serve(settings(), () => HttpResponse.error());
    await open();

    await type(retention(), "12");
    await userEvent.click(button("Save changes"));

    await until(() => {
      expect(h.root.querySelector("form .cp-notice")?.textContent).toBe(
        "Settings were not saved. grelot did not answer. Try again in a moment.Retry",
      );
    });
  });

  it.each([502, 504])(
    "offers Retry on a save a proxy answered %i with no body, and Retry sends it again",
    async (status) => {
      const sent = serve(
        settings(),
        () => new HttpResponse(null, { status }),
        () => HttpResponse.json(settings({ revision: 4, retention_ms: 12 * hour })),
      );
      await open();

      await type(retention(), "12");
      await userEvent.click(button("Save changes"));

      const notice = await until(() => {
        const n = h.root.querySelector<HTMLElement>("form .cp-notice");
        expect(n?.textContent).toBe(
          "Settings were not saved. grelot did not answer. Try again in a moment.Retry",
        );
        return n;
      });
      await userEvent.click(button("Retry", notice ?? undefined));

      await until(() => {
        expect(savebar()?.hidden).toBe(true);
      });
      expect(sent).toHaveLength(2);
      expect(sent[1]).toEqual(sent[0]);
    },
  );

  it("offers Retry on a rate-limited save once the server's wait has passed", async () => {
    const sent = serve(
      settings(),
      () =>
        HttpResponse.json(
          { code: "rate_limited", error: "Too many requests.", retry_after_ms: 1000 },
          { status: 429, headers: { "Retry-After": "1" } },
        ),
      () => HttpResponse.json(settings({ revision: 4, retention_ms: 12 * hour })),
    );
    await open();

    await type(retention(), "12");
    await userEvent.click(button("Save changes"));

    const offeredAtOnce = await until(() => {
      expect(h.root.querySelector("form .cp-notice p")?.textContent).toBe(
        "Settings were not saved. Too many requests.",
      );
      return retries().length > 0;
    });
    expect(offeredAtOnce).toBe(false);
    const retry = await until(() =>
      button("Retry", h.root.querySelector("form .cp-notice") ?? undefined),
    );
    await userEvent.click(retry);
    await until(() => {
      expect(savebar()?.hidden).toBe(true);
    });
    expect(sent).toHaveLength(2);
  });

  it.each([
    [500, "internal_error", "The accounts file could not be written."],
    [403, "person_reserved", "Only a person signed in can change the sign-in methods."],
  ])("says a %i %s save in the server's words with no Retry", async (status, code, words) => {
    const sent = serve(settings(), refusal(status, code, words));
    await open();

    await type(retention(), "12");
    await userEvent.click(button("Save changes"));

    await until(() => {
      expect(h.root.querySelector("form .cp-notice")?.textContent).toBe(
        `Settings were not saved. ${words}`,
      );
    });
    expect(retries()).toEqual([]);
    expect(sent).toHaveLength(1);
    expect(savebar()?.textContent).toMatch(/^1 unsaved change/);
  });

  it("asks before leaving with unsaved changes, and stays on Keep editing", async () => {
    serve(settings());
    await open();
    await type(retention(), "10");

    h.root.querySelector<HTMLAnchorElement>('a.cp-nav-item[href="/statistics"]')?.click();
    const asked = await until(() => {
      const q = question();
      expect(q?.querySelector("h2")?.textContent).toBe("Discard unsaved changes?");
      return q;
    });
    expect(location.pathname).toBe("/settings");
    await userEvent.click(button("Keep editing", asked ?? undefined));

    await until(() => {
      expect(question()).toBeNull();
    });
    expect(location.pathname).toBe("/settings");
    expect(retention().value).toBe("10");
  });

  it("leaves on Discard", async () => {
    serve(settings());
    await open();
    await type(retention(), "10");

    h.root.querySelector<HTMLAnchorElement>('a.cp-nav-item[href="/statistics"]')?.click();
    const asked = await until(() => {
      const q = question();
      expect(q).not.toBeNull();
      return q;
    });
    await userEvent.click(button("Discard", asked ?? undefined));

    await until(() => {
      expect(location.pathname).toBe("/statistics");
    });
  });

  it("asks on Back with unsaved changes, and keeps the address on Keep editing", async () => {
    serve(settings());
    history.replaceState(null, "", "/messages");
    const router = start("/messages", { settings: mountSettings });
    router.go({ screen: "settings", section: null });
    await until(() => {
      expect(retention().value).toBe("72");
    });
    await type(retention(), "10");

    history.back();
    const asked = await until(() => {
      const q = question();
      expect(q).not.toBeNull();
      return q;
    });
    expect(location.pathname).toBe("/settings");
    await userEvent.click(button("Keep editing", asked ?? undefined));
    expect(location.pathname).toBe("/settings");
    expect(retention().value).toBe("10");

    history.back();
    const again = await until(() => {
      const q = question();
      expect(q).not.toBeNull();
      return q;
    });
    await userEvent.click(button("Discard", again ?? undefined));
    await until(() => {
      expect(location.pathname).toBe("/messages");
    });
  });

  it("leaves without asking when nothing is unsaved", async () => {
    serve(settings());
    await open();

    h.root.querySelector<HTMLAnchorElement>('a.cp-nav-item[href="/statistics"]')?.click();

    expect(location.pathname).toBe("/statistics");
    expect(question()).toBeNull();
  });

  it("says a failed load and loads on Retry", async () => {
    let calls = 0;
    worker.use(
      http.get("/api/settings", () => {
        calls++;
        return calls === 1
          ? HttpResponse.json(
              { code: "timeout", error: "The accounts file is busy." },
              { status: 503 },
            )
          : HttpResponse.json(settings());
      }),
    );
    start("/settings", { settings: mountSettings });

    const notice = await until(() => {
      const n = h.root.querySelector<HTMLElement>(".cp-notice[data-tone]");
      expect(n?.textContent).toBe("Settings could not be loaded. The accounts file is busy.Retry");
      return n;
    });
    expect(h.root.querySelector<HTMLElement>(".cp-settings")?.hidden).toBe(true);
    await userEvent.click(button("Retry", notice ?? undefined));

    await until(() => {
      expect(retention().value).toBe("72");
    });
    expect(h.root.querySelector(".cp-notice[data-tone]")).toBeNull();
  });
});
