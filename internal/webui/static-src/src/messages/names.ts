import { el } from "@cplieger/reactive/dom";

import { agentHref } from "../router.js";
import type { Thread } from "../wire/types.gen.js";

/** The reserved name the user posts as (internal/store userName). */
const userName = "user";

type Named = Pick<Thread, "kind" | "title" | "path" | "agents">;

/** Whether name is the user's own, @user. */
export function isUser(name: string): boolean {
  return name === userName;
}

/** How a sender is named on screen: the user's own posts read "You". */
export function senderWords(name: string): string {
  return name === userName ? "You" : `@${name}`;
}

/** A sender's or recipient's name, linked to its agent page when it has one. */
export function agentLink(name: string): HTMLElement {
  const href = agentHref(name);
  return href === null
    ? el("span", { dir: "auto" }, senderWords(name))
    : el("a", { href, dir: "auto" }, senderWords(name));
}

/** A conversation's title: the path, "Announcements", or its two sides. */
export function conversationTitle(t: Named): string {
  switch (t.kind) {
    case "announce":
      return "Announcements";
    case "path":
      return t.path ?? t.title;
    case "direct": {
      const [a, b, ...rest] = t.agents ?? [];
      if (a === undefined || b === undefined || rest.length > 0) {
        return t.title;
      }
      if (a === userName || b === userName) {
        return `You and @${a === userName ? b : a}`;
      }
      return `@${a} and @${b}`;
    }
  }
}

/** The kind's chip, or null for announce, whose title already names it. */
export function kindWords(kind: Thread["kind"]): string | null {
  switch (kind) {
    case "announce":
      return null;
    case "path":
      return "Thread";
    case "direct":
      return "Direct";
  }
}
