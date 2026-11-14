import { el } from "@cplieger/reactive/dom";
import { enhanceCombobox } from "@cplieger/ui/combobox";
import { closeDialog } from "@cplieger/ui/dialog";
import { clearError, showError, type FieldError } from "@cplieger/ui/form";
import { toast } from "@cplieger/ui/toast";

import { agentHref } from "../router.js";
import { newIdempotencyKey } from "../transport.js";
import { createPost, listAgents } from "../wire/client.gen.js";
import type { Agent, Posted, PostTarget } from "../wire/types.gen.js";
import { checkPost, checkOnLeave, composeFields, failureError, type Draft } from "./compose.js";
import { isUser } from "./names.js";

interface NewMessageOptions {
  readonly signal: AbortSignal;
  /** The post was stored and the dialog has closed: open its conversation. */
  readonly onSent: (posted: Posted) => void;
}

interface NewMessage {
  readonly dialog: HTMLDialogElement;
  /** Opens the dialog, To filled with agent name when given. */
  open(name?: string): void;
}

interface Recipient {
  readonly to: PostTarget;
  /** How the toast names it. */
  readonly words: string;
  readonly dormant: boolean;
}

const announcements = "Announcements";

const query = (text: string): string => text.trim().toLowerCase().replace(/^@/, "");

/** Who To names, or the sentence that says how to fix it. */
function recipient(text: string, agents: ReadonlyMap<string, Agent>): Recipient | string {
  const typed = text.trim();
  if (typed === "") {
    return "Choose who gets it: an agent, or Announcements.";
  }
  if (typed.toLowerCase() === announcements.toLowerCase()) {
    return { to: { announce: true }, words: announcements, dormant: false };
  }
  const name = query(typed);
  if (isUser(name)) {
    return "Choose an agent: @user is you.";
  }
  if (agentHref(name) === null) {
    return "Choose an agent from the list, or Announcements.";
  }
  return { to: { agent: name }, words: `@${name}`, dormant: agents.get(name)?.state === "dormant" };
}

function option(label: string, end?: string): HTMLElement {
  const li = el("li", { role: "option", "data-label": label }, el("bdi", null, label));
  if (end !== undefined) {
    li.append(el("small", null, end));
  }
  return li;
}

/**
 * The New message dialog (shape 1 item 7): To over the agents and
 * Announcements, then Subject and Message. A failed send keeps the text,
 * and a failed or cancelled one keeps its key for the same post.
 */
export function newMessageDialog(opts: NewMessageOptions): NewMessage {
  const { signal } = opts;
  const fields = composeFields("new-message", signal);
  checkOnLeave(fields, signal);

  const to = el("input", {
    className: "cp-input",
    id: "new-message-to",
    name: "to",
    autocomplete: "off",
    "aria-required": "true",
    "aria-describedby": "new-message-to-help",
  }) as HTMLInputElement;
  const list = el("ul", { role: "listbox", "aria-label": "Recipients" });
  const empty = el("p", { className: "cp-listbox-empty", hidden: true });
  const popup = el("div", { className: "cp-popover cp-listbox" }, list, empty);
  const cancel = el("button", { className: "cp-button", type: "button" }, "Cancel");
  const form = el(
    "form",
    { novalidate: "" },
    el("header", null, el("h2", { id: "new-message-title" }, "New message")),
    el(
      "div",
      { className: "cp-dialog-body" },
      fields.summary,
      el(
        "div",
        { className: "cp-field" },
        el("label", { className: "cp-label", htmlFor: to.id }, "To"),
        to,
        el(
          "p",
          { className: "cp-help", id: "new-message-to-help" },
          "An agent's name, or Announcements.",
        ),
        popup,
      ),
      fields.subjectField,
      fields.bodyField,
    ),
    el("footer", null, cancel, fields.submit),
  ) as HTMLFormElement;
  const dialog = el(
    "dialog",
    {
      className: "cp-dialog",
      id: "new-message",
      "aria-labelledby": "new-message-title",
      "data-keep-typed": "",
    },
    form,
  ) as HTMLDialogElement;

  let agents = new Map<string, Agent>();
  let loaded: "loading" | "loaded" | "failed" = "loading";
  let loading: AbortController | null = null;
  /** The last send not seen through to its toast, whose key a send of the
   *  same post reuses: the server may have stored a cancelled one. */
  let unfinished: { readonly what: string; readonly key: string } | null = null;
  /** The send in flight. Cancel, Escape and every close abort it. */
  let sending: AbortController | null = null;
  /** The current send's toast and navigation, for its own close as sent. */
  let sent: (() => void) | null = null;
  /** The dialog is on its way out, which a scripted exit plays for 160ms
   *  with Send back at rest: a submit then would be dropped at the close. */
  let closing = false;
  const leave = (): void => {
    closing = true;
    sending?.abort();
  };

  const combobox = enhanceCombobox(to, list, {
    popup,
    empty,
    autoActivate: true,
    onQuery: (text) => {
      const q = query(text);
      for (const o of list.children) {
        if (o instanceof HTMLElement) {
          o.hidden = q !== "" && !(o.dataset["label"] ?? "").toLowerCase().includes(q);
        }
      }
      empty.textContent =
        loaded === "failed"
          ? "The agents could not be loaded. Type an agent's name."
          : `No agent is called “${text.trim()}”.`;
    },
    onAccept: (o) => {
      to.value = o.dataset["label"] ?? "";
      clearError(to);
    },
    signal,
  });

  const fill = (found: readonly Agent[]): void => {
    agents = new Map(found.map((a) => [a.name, a]));
    list.replaceChildren(
      option(announcements),
      ...found
        .filter((a) => !isUser(a.name))
        .map((a) => option(`@${a.name}`, a.state === "dormant" ? "dormant" : undefined)),
    );
    if (to.getAttribute("aria-expanded") === "true") {
      combobox.refresh();
    }
  };

  const load = (): void => {
    loading?.abort();
    const stop = new AbortController();
    loading = stop;
    loaded = "loading";
    fill([]);
    void listAgents({ signal: AbortSignal.any([signal, stop.signal]) }).then((r) => {
      if (stop.signal.aborted) {
        return;
      }
      loaded = r.ok ? "loaded" : "failed";
      fill(r.ok ? r.data.agents : []);
    });
  };

  let entered = "";
  to.addEventListener(
    "focus",
    () => {
      entered = to.value;
    },
    { signal },
  );
  to.addEventListener(
    "blur",
    () => {
      const who = recipient(to.value, agents);
      if (typeof who === "string" && to.value !== entered) {
        showError(to, who);
      } else if (typeof who !== "string") {
        clearError(to);
      }
    },
    { signal },
  );
  to.addEventListener(
    "input",
    () => {
      if (
        to.getAttribute("aria-invalid") === "true" &&
        typeof recipient(to.value, agents) !== "string"
      ) {
        clearError(to);
      }
    },
    { signal },
  );

  cancel.addEventListener(
    "click",
    () => {
      leave();
      closeDialog(dialog, "cancel");
    },
    { signal },
  );
  dialog.addEventListener("cancel", leave, { signal });
  // returnValue tells this send's own close from any other.
  dialog.addEventListener(
    "close",
    () => {
      const done = dialog.returnValue === "sent" ? sent : null;
      sent = null;
      sending?.abort();
      done?.();
    },
    { signal },
  );

  form.addEventListener(
    "submit",
    (e) => {
      e.preventDefault();
      if (closing) {
        return;
      }
      const who = recipient(to.value, agents);
      const errors: FieldError[] = [
        ...(typeof who === "string" ? [{ field: to, message: who }] : []),
        ...checkPost(fields),
      ];
      fields.report(form, errors);
      if (typeof who === "string" || errors.length > 0) {
        return;
      }
      const d: Draft = fields.draft();
      const what = JSON.stringify([who.to, d]);
      const key = unfinished?.what === what ? unfinished.key : newIdempotencyKey();
      void fields.send.run(async () => {
        unfinished = { what, key };
        const mine = new AbortController();
        sending = mine;
        const result = await createPost(
          { body: { to: who.to, subject: d.subject, body: d.body }, idempotencyKey: key },
          { signal: AbortSignal.any([signal, mine.signal]) },
        );
        if (mine.signal.aborted || (!result.ok && result.code === "aborted")) {
          return;
        }
        if (!result.ok) {
          fields.report(form, [failureError(result, fields, to)]);
          return;
        }
        sent = () => {
          unfinished = null;
          toast({
            kind: "success",
            title: `Sent to ${who.words}.`,
            ...(who.dormant
              ? { body: `${who.words} is dormant and reads it once it registers again.` }
              : {}),
          });
          opts.onSent(result.data);
        };
        closing = true;
        closeDialog(dialog, "sent");
      });
    },
    { signal },
  );

  return {
    dialog,
    open(name) {
      to.defaultValue = name === undefined ? "" : `@${name}`;
      form.reset();
      fields.body.dispatchEvent(new Event("input", { bubbles: true }));
      fields.report(form, []);
      fields.subject.autofocus = name !== undefined;
      // A native Escape close leaves returnValue as it was.
      dialog.returnValue = "";
      closing = false;
      load();
      dialog.showModal();
    },
  };
}
