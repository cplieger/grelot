import { el } from "@cplieger/reactive/dom";
import { announce } from "@cplieger/ui/announce";
import { ask } from "@cplieger/ui/ask";
import { enhanceConfirm } from "@cplieger/ui/confirm";
import { clearError, showError } from "@cplieger/ui/form";
import { toast } from "@cplieger/ui/toast";

import { icon } from "../icons.js";
import { retryButton } from "../messages/parts.js";
import { reader, type Answer } from "../messages/reader.js";
import type { SettingsSection } from "../router.js";
import type { ScreenMount } from "../shell.js";
import { failureWords } from "../transport.js";
import { getSettings, putSettings } from "../wire/client.gen.js";
import type { Methods, Settings, SettingsRequest } from "../wire/types.gen.js";

type PutResult = Awaited<ReturnType<typeof putSettings>>;
type Refusal = Extract<PutResult, { readonly ok: false }>;

const hour = 3_600_000;

const categories: readonly (readonly [SettingsSection, string])[] = [
  ["history", "History"],
  ["sign-in", "Sign-in"],
];

const oidcSet = "Sign in through the identity provider the server names.";
const methodRows: readonly (readonly [keyof Methods, string, string])[] = [
  ["password", "Passwords", "Sign in with a username and a password."],
  ["passkeys", "Passkeys", "Sign in with a passkey saved on a device or a security key."],
  ["oidc", "Single sign-on", oidcSet],
];

const oidcUnset = "Set OIDC_ISSUER, OIDC_CLIENT_ID and OIDC_CLIENT_SECRET to offer it.";
const lockOutWords = "You would have no way to sign in. Turn on another method first.";
const recentAuthWords = "Sign in again to change sign-in methods.";

function changesWords(n: number): string {
  return n === 1 ? "1 unsaved change" : `${String(n)} unsaved changes`;
}

/** A .cp-setting row: label, help and the field's error at the start, the control at the end. */
function settingRow(
  control: HTMLInputElement,
  label: string,
  help: HTMLElement,
): { readonly row: HTMLElement; readonly label: HTMLElement } {
  const labelEl = el("label", { className: "cp-label", htmlFor: control.id }, label);
  const error = el("span", {
    className: "cp-field-error",
    id: `${control.id}-error`,
    hidden: true,
  });
  control.setAttribute("aria-describedby", help.id);
  return {
    row: el("div", { className: "cp-setting" }, el("div", null, labelEl, help, error), control),
    label: labelEl,
  };
}

function notice(tone: "danger" | "warning", words: string, retry?: () => void): HTMLElement {
  return el(
    "div",
    { className: "cp-notice", "data-tone": tone },
    icon(tone === "danger" ? "circle-x" : "triangle-alert"),
    el("p", null, words),
    ...(retry === undefined ? [] : [retryButton(retry)]),
  );
}

/**
 * The Settings screen (shape 4): the history retention and the sign-in
 * methods, each category at its own address, saved together with Save.
 * Leaving the screen with unsaved changes asks first.
 */
export const mountSettings: ScreenMount<"settings"> = (host, first, ctx) => {
  const { signal, router } = ctx;
  let route = first;
  let saved: Settings | null = null;

  const retention = el("input", {
    className: "cp-input",
    id: "settings-retention",
    type: "number",
    inputMode: "numeric",
    step: "1",
  }) as HTMLInputElement;
  const retentionHelp = el(
    "span",
    { className: "cp-help", id: "settings-retention-help" },
    "Threads, messages and events are deleted this long after they are written. Announcements are kept for 7 days whatever this says. A change applies to what is written from now on.",
  );
  const retentionRow = settingRow(retention, "Keep history for (hours)", retentionHelp);

  const boxes = new Map<keyof Methods, HTMLInputElement>();
  const helps = new Map<keyof Methods, HTMLElement>();
  const methodEls = methodRows.map(([key, label, help]) => {
    const box = el("input", {
      className: "cp-checkbox",
      id: `settings-${key}`,
      type: "checkbox",
    }) as HTMLInputElement;
    const helpEl = el("span", { className: "cp-help", id: `settings-${key}-help` }, help);
    boxes.set(key, box);
    helps.set(key, helpEl);
    return settingRow(box, label, helpEl).row;
  });

  const group = (id: string, heading: string, ...rows: HTMLElement[]): HTMLElement =>
    el(
      "section",
      { className: "cp-stack", "aria-labelledby": id },
      el("h3", { id }, heading),
      el("div", { className: "cp-panel" }, ...rows),
    );
  const headings = new Map<SettingsSection, HTMLElement>();
  const bodies = new Map<SettingsSection, HTMLElement>();
  const groups: Readonly<Record<SettingsSection, HTMLElement>> = {
    history: group("settings-retention-title", "Retention", retentionRow.row),
    "sign-in": group("settings-methods-title", "Sign-in methods", ...methodEls),
  };
  for (const [section, words] of categories) {
    const h2 = el("h2", { tabIndex: -1 }, words);
    headings.set(section, h2);
    bodies.set(section, el("div", { className: "cp-stack" }, h2, groups[section]));
  }

  const links = new Map<SettingsSection, HTMLAnchorElement>();
  const nav = el(
    "nav",
    { className: "cp-settings-nav", "aria-label": "Settings" },
    el(
      "ul",
      {
        className: "cp-list",
        "data-surface": "plain",
        "data-divider": "none",
        "data-density": "compact",
      },
      ...categories.map(([section, words]) => {
        const a = el(
          "a",
          { className: "cp-list-row", href: `/settings/${section}` },
          el("span", { className: "cp-cell-stack" }, words),
        ) as HTMLAnchorElement;
        links.set(section, a);
        return el("li", null, a);
      }),
    ),
  );

  const formNotice = el("div");
  const count = el("span");
  const discard = el("button", { className: "cp-button", type: "button" }, "Discard");
  const busy = icon("spinner-arc");
  busy.setAttribute("data-part", "busy");
  const saveButton = el(
    "button",
    { className: "cp-button", "data-variant": "primary", type: "submit" },
    busy,
    el("span", { className: "cp-roll" }, "Save changes"),
  ) as HTMLButtonElement;
  const saving = enhanceConfirm(saveButton, { busy: "Saving…", done: false, signal });
  const savebar = el(
    "section",
    { className: "cp-savebar", "aria-label": "Unsaved changes", hidden: true },
    count,
    discard,
    saveButton,
  );
  const form = el(
    "form",
    { className: "cp-stack", noValidate: true },
    formNotice,
    ...bodies.values(),
    savebar,
  ) as HTMLFormElement;
  const page = el("div", { className: "cp-settings", hidden: true }, nav, form);
  const skeleton = el(
    "div",
    { className: "cp-stack", hidden: true },
    ...Array.from({ length: 3 }, () => el("span", { className: "cp-skeleton" })),
  );
  const loadNotice = el("div");
  host.append(
    el("div", { className: "cp-heading-row" }, el("h1", null, "Settings")),
    loadNotice,
    skeleton,
    page,
  );

  const open = (): SettingsSection => route.section ?? "history";
  const show = (): void => {
    for (const [section, body] of bodies) {
      body.hidden = section !== open();
    }
    for (const [section, a] of links) {
      if (section === open()) {
        a.setAttribute("aria-current", "page");
      } else {
        a.removeAttribute("aria-current");
      }
    }
  };
  const goTo = (section: SettingsSection): void => {
    if (open() !== section) {
      router.go({ screen: "settings", section });
    }
  };

  /** The typed retention in milliseconds, or null when the field holds no number. */
  const typedRetention = (): number | null => {
    const n = retention.value.trim() === "" ? Number.NaN : Number(retention.value);
    return Number.isFinite(n) ? n * hour : null;
  };
  const retentionChanged = (): boolean => saved !== null && typedRetention() !== saved.retention_ms;
  const changedMethods = (): (keyof Methods)[] =>
    saved === null
      ? []
      : [...boxes].filter(([key, box]) => box.checked !== saved?.methods[key]).map(([k]) => k);
  const pending = (): number => (retentionChanged() ? 1 : 0) + changedMethods().length;

  const limitsWords = (s: Settings): string =>
    `Enter a whole number of hours from ${String(s.retention_min_ms / hour)} to ${String(s.retention_max_ms / hour)}.`;
  /** The retention field's error, or null when it is unchanged or valid. */
  const retentionError = (): string | null => {
    if (saved === null || !retentionChanged()) {
      return null;
    }
    const ms = typedRetention();
    const fits =
      ms !== null &&
      Number.isInteger(ms / hour) &&
      ms >= saved.retention_min_ms &&
      ms <= saved.retention_max_ms;
    return fits ? null : limitsWords(saved);
  };

  const dot = (on: boolean): HTMLElement[] =>
    on
      ? [
          el(
            "span",
            { "data-part": "end" },
            el("span", { className: "cp-dot", "data-tone": "accent", "aria-hidden": "true" }),
            el("span", { className: "cp-visually-hidden" }, ", unsaved changes"),
          ),
        ]
      : [];
  const words = new Map(categories);
  /** Puts the savebar and the categories' marks in step with the fields. */
  const refresh = (): void => {
    const n = pending();
    if (n === 0 && savebar.contains(document.activeElement)) {
      headings.get(open())?.focus();
    }
    savebar.hidden = n === 0;
    count.textContent = changesWords(n);
    const marks: Readonly<Record<SettingsSection, boolean>> = {
      history: retentionChanged(),
      "sign-in": changedMethods().length > 0,
    };
    for (const [section, a] of links) {
      a.replaceChildren(
        el("span", { className: "cp-cell-stack" }, words.get(section) ?? ""),
        ...dot(marks[section]),
      );
    }
  };

  const clearAll = (): void => {
    clearError(retention);
    for (const box of boxes.values()) {
      clearError(box);
    }
    formNotice.replaceChildren();
  };

  /** Puts s in the fields, as stored. */
  const fill = (s: Settings): void => {
    saved = s;
    retention.min = String(s.retention_min_ms / hour);
    retention.max = String(s.retention_max_ms / hour);
    retention.value = String(s.retention_ms / hour);
    for (const [key, box] of boxes) {
      box.checked = s.methods[key];
    }
    const oidc = boxes.get("oidc");
    const oidcHelp = helps.get("oidc");
    if (oidc !== undefined && oidcHelp !== undefined) {
      oidc.disabled = !s.oidc_configured;
      oidcHelp.textContent = s.oidc_configured ? oidcSet : oidcUnset;
    }
    clearAll();
    refresh();
  };

  form.addEventListener("input", refresh, { signal });
  form.addEventListener("change", refresh, { signal });
  retention.addEventListener(
    "blur",
    () => {
      const error = retentionError();
      if (error === null) {
        clearError(retention);
      } else {
        showError(retention, error);
      }
    },
    { signal },
  );
  retention.addEventListener(
    "input",
    () => {
      if (retention.hasAttribute("aria-invalid") && retentionError() === null) {
        clearError(retention);
      }
    },
    { signal },
  );
  for (const box of boxes.values()) {
    box.addEventListener(
      "change",
      () => {
        for (const b of boxes.values()) {
          clearError(b);
        }
      },
      { signal },
    );
  }

  discard.addEventListener(
    "click",
    () => {
      if (saved !== null) {
        fill(saved);
      }
    },
    { signal },
  );

  const refused = (base: Settings, result: Refusal): void => {
    switch (result.code) {
      case "aborted":
        return;
      case "invalid":
        goTo("history");
        showError(retention, limitsWords(base));
        retention.focus();
        return;
      case "would_lock_out": {
        goTo("sign-in");
        const off = [...boxes].find(([key, box]) => base.methods[key] && !box.checked);
        const box = off?.[1] ?? boxes.get("password");
        if (box !== undefined) {
          showError(box, lockOutWords);
          box.focus();
        }
        return;
      }
      case "recent_auth_required":
        formNotice.replaceChildren(notice("warning", recentAuthWords));
        announce(recentAuthWords);
        return;
      default: {
        const words = `Settings were not saved. ${failureWords(result)}`;
        formNotice.replaceChildren(
          notice("danger", words, () => {
            form.requestSubmit(saveButton);
          }),
        );
        announce(words);
      }
    }
  };

  const save = async (): Promise<void> => {
    const base = saved;
    if (base === null || pending() === 0) {
      return;
    }
    const error = retentionError();
    if (error !== null) {
      goTo("history");
      showError(retention, error);
      retention.focus();
      return;
    }
    formNotice.replaceChildren();
    const methods = { ...base.methods };
    for (const [key, box] of boxes) {
      methods[key] = box.checked;
    }
    const body: SettingsRequest = {
      retention_ms: retentionChanged()
        ? (typedRetention() ?? base.retention_ms)
        : base.retention_ms,
      methods,
    };
    const sent: { answer?: PutResult } = {};
    await saving.run(async () => {
      sent.answer = await putSettings({ body }, { signal });
    });
    const { answer } = sent;
    if (answer === undefined) {
      return;
    }
    if (!answer.ok) {
      refused(base, answer);
      return;
    }
    fill(answer.data);
    toast({ kind: "success", title: "Settings saved." });
  };
  form.addEventListener(
    "submit",
    (e) => {
      e.preventDefault();
      void save();
    },
    { signal },
  );

  router.hold(
    {
      holds: (next) => next.screen !== "settings" && pending() > 0,
      ask: () =>
        ask("Discard unsaved changes?", {
          confirmLabel: "Discard",
          cancelLabel: "Keep editing",
          variant: "destructive",
        }),
    },
    signal,
  );
  window.addEventListener(
    "beforeunload",
    (e) => {
      if (pending() > 0) {
        e.preventDefault();
      }
    },
    { signal },
  );

  const fetchSettings = async (asking: AbortSignal): Promise<Answer> => {
    const result = await getSettings({ signal: asking });
    if (result.ok) {
      return {
        draw: () => {
          loadNotice.replaceChildren();
          skeleton.hidden = true;
          page.hidden = false;
          fill(result.data);
          return false;
        },
      };
    }
    return result.code === "aborted" ? null : { failure: result };
  };
  const settings = reader(signal, {
    holds: () => saved !== null,
    load: fetchSettings,
    refresh: fetchSettings,
    skeleton: () => {
      loadNotice.replaceChildren();
      skeleton.hidden = false;
    },
    failed: (failure) => {
      skeleton.hidden = true;
      loadNotice.replaceChildren(
        notice("danger", `Settings could not be loaded. ${failureWords(failure)}`, () => {
          void settings.retry();
        }),
      );
    },
  });
  void settings.load();

  show();
  return {
    update: (next) => {
      if (next.screen !== "settings") {
        return;
      }
      route = next;
      show();
    },
  };
};
