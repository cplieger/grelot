import { el } from "@cplieger/reactive/dom";
import { showError } from "@cplieger/ui/form";

import { newIdempotencyKey, type ApiResult } from "../transport.js";
import { createPost } from "../wire/client.gen.js";
import type { Post, PostTarget } from "../wire/types.gen.js";
import {
  checkOnLeave,
  checkPost,
  composeFields,
  failureError,
  saveDraft,
  savedDraft,
  type Draft,
} from "./compose.js";
import type { Pending } from "./feed.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

/** The composer on screen for a conversation, which a send reports to. */
interface Mounted {
  readonly drawPending: () => void;
  readonly sent: (d: Draft) => void;
  readonly failed: (f: Failure) => void;
}

/** One conversation's sends, kept for the page's life. */
interface Sends {
  pending: Pending[];
  seq: number;
  /** Each entry's Idempotency-Key, reused by its Retry so the server stores it once. */
  readonly keys: Map<number, string>;
  /** Each failed entry's refusal, which a composer drawn later still shows. */
  readonly refusals: Map<number, Failure>;
  readonly flying: Map<number, Promise<void>>;
  /** How many of its posts the server has stored so far. */
  stored: number;
  /** Each sent entry's stored count once the server stored it. */
  readonly storedAt: Map<number, number>;
  mounted: Mounted | null;
}

/**
 * Every conversation's sends by id, one store per page. It outlives each
 * composer, since a send outlives a switch to another conversation or screen
 * and the composer drawn on return must show it, still sending or failed
 * with its first key.
 */
export type SendStore = Map<number, Sends>;

export const sendStore = (): SendStore => new Map();

/** Conversation id's stored count: take it before a read, and pass it to settle with that read's posts. */
export const storedCount = (store: SendStore, id: number): number => store.get(id)?.stored ?? 0;

interface ComposerOptions {
  /** The conversation, whose draft this tab keeps. */
  readonly id: number;
  readonly to: PostTarget;
  readonly sends: SendStore;
  /** The open conversation's life: the form's listeners and its redraws. */
  readonly signal: AbortSignal;
  /** The page's life: a send outlives a switch to another conversation or screen. */
  readonly page: AbortSignal;
  /** The pending entries changed: draw them. */
  readonly onPending: () => void;
  /** A post was stored: fetch the conversation, which now holds it. */
  readonly onSent: () => void;
}

interface Composer {
  readonly form: HTMLFormElement;
  /** The user's posts on their way, oldest first. */
  pending(): readonly Pending[];
  /** Call once form is in the document: shows why the server refused the post its fields hold. */
  shown(): void;
  /** Sends failed entry n again, with the key it was first sent with. */
  retry(n: number): void;
  /**
   * Drops each sent entry whose post is among posts, or was stored before
   * the read of posts began (seen, storedCount then), since that read lacks
   * it only once it expired or lies before posts. Kept, it would show again
   * as the user's newest post.
   */
  settle(posts: readonly Post[], seen: number): void;
}

const nothing: Draft = { subject: "", body: "" };

const same = (a: Draft | null, b: Draft): boolean =>
  a !== null && a.subject === b.subject && a.body === b.body;

/**
 * The open conversation's composer (shape 1 item 7, design-patterns.md
 * "Messages" items 4 and 5): each post shows at once as the user's own
 * entry, sending, then sent or failed with Retry, and what was typed stays
 * until it is stored.
 */
export function conversationComposer(opts: ComposerOptions): Composer {
  const { id, signal } = opts;
  const fields = composeFields("compose", signal);
  const form = el(
    "form",
    { className: "cp-form", novalidate: "" },
    fields.summary,
    fields.subjectField,
    fields.bodyField,
    el("div", null, fields.submit),
  ) as HTMLFormElement;
  checkOnLeave(fields, signal);

  let s = opts.sends.get(id);
  if (s === undefined) {
    s = {
      pending: [],
      seq: 0,
      keys: new Map(),
      refusals: new Map(),
      flying: new Map(),
      stored: 0,
      storedAt: new Map(),
      mounted: null,
    };
    opts.sends.set(id, s);
  }
  const sends = s;
  const mounted: Mounted = {
    drawPending: opts.onPending,
    sent(d) {
      if (same(fields.draft(), d)) {
        fields.fill(nothing);
        fields.body.focus();
      }
      opts.onSent();
    },
    failed(f) {
      fields.report(form, [failureError(f, fields)]);
    },
  };
  sends.mounted = mounted;
  signal.addEventListener("abort", () => {
    if (sends.mounted === mounted) {
      sends.mounted = null;
    }
  });

  const set = (n: number, change: Partial<Pick<Pending, "state" | "message">>): void => {
    sends.pending = sends.pending.map((p) => (p.n === n ? { ...p, ...change } : p));
    sends.mounted?.drawPending();
  };

  const post = async (n: number): Promise<void> => {
    const p = sends.pending.find((x) => x.n === n);
    const key = sends.keys.get(n);
    if (p === undefined || key === undefined) {
      return;
    }
    sends.refusals.delete(n);
    set(n, { state: "sending" });
    const result = await createPost(
      { body: { to: opts.to, subject: p.subject, body: p.body }, idempotencyKey: key },
      { signal: opts.page },
    );
    if (result.ok) {
      sends.storedAt.set(n, ++sends.stored);
      set(n, { state: "sent", message: result.data.message });
      if (same(savedDraft(id), p)) {
        saveDraft(id, nothing);
      }
      sends.mounted?.sent(p);
      return;
    }
    if (result.code === "aborted") {
      return;
    }
    sends.refusals.set(n, result);
    set(n, { state: "failed" });
    sends.mounted?.failed(result);
  };

  /** Sends entry n, or waits on it when it is already on its way. */
  const deliver = (n: number): Promise<void> => {
    let flight = sends.flying.get(n);
    if (flight === undefined) {
      flight = post(n).finally(() => sends.flying.delete(n));
      sends.flying.set(n, flight);
    }
    return flight;
  };

  form.addEventListener(
    "submit",
    (e) => {
      e.preventDefault();
      const errors = checkPost(fields);
      fields.report(form, errors);
      if (errors.length > 0) {
        return;
      }
      const d = fields.draft();
      void fields.send.run(async () => {
        // Send again on the text of a failed entry is that entry's Retry.
        const failed = sends.pending.find((p) => p.state === "failed" && same(p, d));
        let n = failed?.n;
        if (n === undefined) {
          n = ++sends.seq;
          sends.keys.set(n, newIdempotencyKey());
          sends.pending = [
            ...sends.pending,
            { n, ...d, at: Date.now(), state: "sending", message: null },
          ];
        }
        await deliver(n);
      });
    },
    { signal },
  );
  form.addEventListener(
    "input",
    () => {
      saveDraft(id, fields.draft());
    },
    { signal },
  );
  const kept = savedDraft(id);
  if (kept !== null) {
    fields.fill(kept);
  }
  // Send stays busy for a send started before the switch, so no second one starts.
  for (const n of sends.flying.keys()) {
    void fields.send.run(() => deliver(n));
  }

  return {
    form,
    pending: () => sends.pending,
    shown() {
      const d = fields.draft();
      const refused = sends.pending.find((p) => p.state === "failed" && same(d, p));
      const why = refused === undefined ? undefined : sends.refusals.get(refused.n);
      if (why !== undefined) {
        const e = failureError(why, fields);
        showError(e.field, e.message);
      }
    },
    retry(n) {
      // The Retry button leaves with the failed state, so focus goes to the message.
      fields.body.focus();
      void fields.send.run(() => deliver(n));
    },
    settle(posts, seen) {
      const ids = new Set(posts.map((p) => p.id));
      const done = sends.pending.filter((p) => {
        const at = sends.storedAt.get(p.n);
        return p.message !== null && at !== undefined && (ids.has(p.message) || at <= seen);
      });
      if (done.length === 0) {
        return;
      }
      sends.pending = sends.pending.filter((p) => !done.includes(p));
      for (const p of done) {
        sends.keys.delete(p.n);
        sends.storedAt.delete(p.n);
      }
    },
  };
}
