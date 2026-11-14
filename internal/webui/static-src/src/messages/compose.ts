import { el } from "@cplieger/reactive/dom";
import { enhanceConfirm, type Confirm } from "@cplieger/ui/confirm";
import { clearError, reportErrors, showError, type FieldError } from "@cplieger/ui/form";
import { enhanceTextarea, utf8Length } from "@cplieger/ui/textarea";

import { icon } from "../icons.js";
import { draftKey } from "../storage-keys.js";
import { failureWords, type ApiResult } from "../transport.js";

/** The server's subject limit, in characters (internal/store post.go maxSubject). */
const maxSubject = 120;
/** The server's body limit, in UTF-8 bytes (internal/store post.go maxBody). */
const maxBody = 64 * 1024;

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

/** What a person wrote: the subject as it is sent, trimmed, and the body. */
export interface Draft {
  readonly subject: string;
  readonly body: string;
}

/** A post's Subject and Message fields, its error summary and its Send button. */
interface ComposeFields {
  readonly summary: HTMLElement;
  readonly subjectField: HTMLElement;
  readonly bodyField: HTMLElement;
  readonly subject: HTMLInputElement;
  readonly body: HTMLTextAreaElement;
  /** The form's one submit button, "Send". */
  readonly submit: HTMLButtonElement;
  /** Runs one send at a time with Send busy, as @cplieger/ui/confirm does. */
  readonly send: Confirm;
  draft(): Draft;
  /** Puts d in the fields, as if typed. */
  fill(d: Draft): void;
  /** Shows errors on form as the forms pattern asks; an empty list clears them. */
  report(form: HTMLFormElement, errors: readonly FieldError[]): void;
}

function kib(bytes: number): string {
  return `${(bytes / 1024).toFixed(1)}\u00a0KiB`;
}

/** The count under Message, shown only near the server's limit. */
function bodyCountWords(n: number, max = maxBody): string {
  return n > max * 0.9 ? `${kib(n)} of ${String(max / 1024)}\u00a0KiB` : "";
}

/** Builds the fields with ids under prefix; Ctrl+Enter and Cmd+Enter send. */
export function composeFields(prefix: string, signal: AbortSignal): ComposeFields {
  const summary = el(
    "div",
    { className: "cp-notice", "data-tone": "danger", hidden: true },
    icon("circle-x"),
    el("div", { "data-part": "body" }),
  );
  const subject = el("input", {
    className: "cp-input",
    id: `${prefix}-subject`,
    name: "subject",
    autocomplete: "off",
    "aria-required": "true",
  }) as HTMLInputElement;
  const body = el("textarea", {
    className: "cp-input",
    id: `${prefix}-body`,
    name: "body",
    rows: 2,
  }) as HTMLTextAreaElement;
  const count = el("span", { className: "cp-textarea-count", id: `${prefix}-body-count` });
  enhanceTextarea(body, {
    max: maxBody,
    measure: utf8Length,
    count,
    words: bodyCountWords,
    submitOnModEnter: true,
    signal,
  });
  const busy = icon("spinner-arc");
  busy.setAttribute("data-part", "busy");
  const done = icon("check");
  done.setAttribute("data-part", "done");
  const submit = el(
    "button",
    { className: "cp-button", "data-variant": "primary", type: "submit" },
    busy,
    done,
    el("span", { className: "cp-roll" }, "Send"),
  ) as HTMLButtonElement;
  const send = enhanceConfirm(submit, { busy: "Sending…", done: false, signal });

  return {
    summary,
    subjectField: el(
      "div",
      { className: "cp-field" },
      el("label", { className: "cp-label", htmlFor: subject.id }, "Subject"),
      subject,
    ),
    bodyField: el(
      "div",
      { className: "cp-field" },
      el("label", { className: "cp-label", htmlFor: body.id }, "Message"),
      body,
      count,
    ),
    subject,
    body,
    submit,
    send,
    draft: () => ({ subject: subject.value.trim(), body: body.value }),
    fill(d) {
      subject.value = d.subject;
      body.value = d.body;
      subject.dispatchEvent(new Event("input", { bubbles: true }));
      body.dispatchEvent(new Event("input", { bubbles: true }));
    },
    report(form, errors) {
      reportErrors(form, errors, {
        summary,
        heading: (n) => `${String(n)} fields need a fix before this sends`,
      });
    },
  };
}

/** The fixes the server's post rules ask of the fields (internal/store Posting.check). */
export function checkPost(f: ComposeFields): FieldError[] {
  const errors: FieldError[] = [];
  const { subject, body } = f.draft();
  // Code points, as the server's utf8.RuneCountInString counts them.
  const length = Array.from(subject).length;
  if (length === 0) {
    errors.push({ field: f.subject, message: "Enter a subject." });
  } else if (length > maxSubject) {
    errors.push({
      field: f.subject,
      message: `Shorten the subject to ${String(maxSubject)} characters. It has ${String(length)}.`,
    });
  } else if (/\p{Cc}/u.test(subject)) {
    errors.push({
      field: f.subject,
      message: "Remove the tab or control character from the subject.",
    });
  }
  const bytes = utf8Length(body);
  if (bytes > maxBody) {
    errors.push({
      field: f.body,
      message: `Shorten the message to 64\u00a0KiB. It is ${kib(bytes)}.`,
    });
  }
  return errors;
}

/**
 * Checks a field as the person leaves it after changing it, and clears its
 * error as soon as its value passes (design-patterns.md "Forms" items 2 and 5).
 */
export function checkOnLeave(f: ComposeFields, signal: AbortSignal): void {
  for (const field of [f.subject, f.body]) {
    let entered = field.value;
    const error = (): FieldError | undefined => checkPost(f).find((e) => e.field === field);
    field.addEventListener(
      "focus",
      () => {
        entered = field.value;
      },
      { signal },
    );
    field.addEventListener(
      "blur",
      () => {
        if (field.value === entered) {
          return;
        }
        const e = error();
        if (e === undefined) {
          clearError(field);
        } else {
          showError(field, e.message);
        }
      },
      { signal },
    );
    field.addEventListener(
      "input",
      () => {
        if (field.getAttribute("aria-invalid") === "true" && error() === undefined) {
          clearError(field);
        }
      },
      { signal },
    );
  }
}

/**
 * The field a refused post names. The error envelope carries no field, so
 * a refusal of the subject is told by its words; to takes a missing or
 * refused recipient, and the message every other refusal.
 */
export function failureError(f: Failure, fields: ComposeFields, to?: HTMLInputElement): FieldError {
  const words = failureWords(f);
  if (f.code === "invalid" && /\bsubject\b/i.test(words)) {
    return { field: fields.subject, message: words };
  }
  if (to !== undefined && (f.code === "not_found" || f.code === "forbidden")) {
    return { field: to, message: words };
  }
  return { field: fields.body, message: words };
}

function storage(): Storage | null {
  try {
    return sessionStorage;
  } catch {
    return null;
  }
}

/** The draft kept for conversation id in this tab, or null. */
export function savedDraft(id: number): Draft | null {
  let v: unknown;
  try {
    v = JSON.parse(storage()?.getItem(draftKey(id)) ?? "null");
  } catch {
    return null;
  }
  if (typeof v !== "object" || v === null) {
    return null;
  }
  const { subject, body } = v as Record<string, unknown>;
  return typeof subject === "string" && typeof body === "string" ? { subject, body } : null;
}

/** Keeps d for conversation id in this tab; an empty draft removes it. */
export function saveDraft(id: number, d: Draft): void {
  try {
    if (d.subject === "" && d.body === "") {
      storage()?.removeItem(draftKey(id));
    } else {
      storage()?.setItem(draftKey(id), JSON.stringify(d));
    }
  } catch {
    // A full or refused storage keeps the draft in the fields only.
  }
}
