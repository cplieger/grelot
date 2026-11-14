import { el } from "@cplieger/reactive/dom";
import { enhanceContextPanel } from "@cplieger/ui/context-panel";

import { clockTime, ordinal } from "../format.js";
import { icon } from "../icons.js";
import type { Agent, Conversation, Reservation } from "../wire/types.gen.js";
import { agentLink, isUser } from "./names.js";

interface ConversationContext {
  /** The .cp-context: the panel, then the main column. */
  readonly root: HTMLElement;
  /** The .cp-context-main the feed goes in. */
  readonly main: HTMLElement;
  update(c: Conversation): void;
}

function reason(r: Reservation): (Node | string)[] {
  return r.reason === undefined || r.reason === "" ? [] : [": ", el("bdi", null, r.reason)];
}

/** The reservation strip (shape 1 item 6): the grants, then the queue in order. */
function reservationItem(r: Reservation): HTMLElement {
  const path = el("code", { className: "cp-mono", dir: "auto" }, r.path);
  if (r.state === "held") {
    return el(
      "li",
      null,
      agentLink(r.holder),
      " holds ",
      path,
      " until ",
      el("time", { datetime: new Date(r.expires_ms).toISOString() }, clockTime(r.expires_ms)),
      ...reason(r),
    );
  }
  return el(
    "li",
    null,
    agentLink(r.holder),
    `, ${ordinal(r.place ?? 1)} in queue for `,
    path,
    ...reason(r),
  );
}

function stateWords(a: Agent): HTMLElement {
  return a.state === "live"
    ? el("span", { className: "cp-state", "data-tone": "success" }, icon("circle-check"), "Live")
    : el("span", { className: "cp-state", "data-tone": "neutral" }, icon("moon"), "Dormant");
}

/** A direct conversation's other side: its card, or a line when it no longer exists. */
function cardSection(name: string, card: Agent | undefined): HTMLElement {
  if (card === undefined) {
    return el(
      "section",
      null,
      el("h3", { dir: "auto" }, `@${name}`),
      el("p", { className: "cp-meta" }, "No longer registered."),
    );
  }
  const section = el("section", null, el("h3", null, agentLink(card.name)));
  if (card.description !== "") {
    section.append(el("p", { dir: "auto" }, card.description));
  }
  section.append(el("p", null, stateWords(card)));
  if (card.host !== undefined && card.host !== "") {
    section.append(el("p", { className: "cp-meta" }, "Machine: ", el("bdi", null, card.host)));
  }
  return section;
}

interface Part {
  readonly key: string;
  readonly draw: () => HTMLElement;
}

function pathParts(c: Conversation): Part[] {
  return [
    {
      key: JSON.stringify(c.reservations),
      draw: () =>
        el(
          "section",
          null,
          el("h3", null, "Reservations"),
          c.reservations.length === 0
            ? el("p", { className: "cp-meta" }, "No reservations.")
            : el("ol", null, ...c.reservations.map(reservationItem)),
        ),
    },
    {
      key: JSON.stringify(c.followers),
      draw: () =>
        el(
          "section",
          null,
          el("h3", null, "Followers"),
          c.followers.length === 0
            ? el("p", { className: "cp-meta" }, "No followers.")
            : el("ul", null, ...c.followers.map((f) => el("li", null, agentLink(f)))),
        ),
    },
  ];
}

/** The agents across from the user, or both sides of two agents' conversation. */
function otherSides(c: Conversation): string[] {
  return (c.thread.agents ?? []).filter((n) => !isUser(n));
}

function directParts(c: Conversation): Part[] {
  return otherSides(c).map((name) => {
    const card = c.cards.find((a) => a.name === name);
    return { key: JSON.stringify(card ?? name), draw: () => cardSection(name, card) };
  });
}

function summaryWords(c: Conversation): string {
  if (c.thread.kind === "path") {
    return "Reservations, queue and followers";
  }
  const names = otherSides(c).map((n) => `@${n}`);
  return `About ${new Intl.ListFormat("en", { type: "conjunction" }).format(names)}`;
}

/**
 * What surrounds an open conversation (@cplieger/ui/context-panel): a path
 * thread's reservations, queue and followers, a direct thread's cards; null
 * for the announcement thread, which has neither.
 */
export function conversationContext(
  first: Conversation,
  signal: AbortSignal,
): ConversationContext | null {
  if (first.thread.kind === "announce") {
    return null;
  }
  const summary = el("summary", null, summaryWords(first));
  const panel = el("details", { className: "cp-context-panel" }, summary);
  const main = el("div", { className: "cp-context-main" });
  const root = el("div", { className: "cp-context" }, panel, main);
  let drawn: { key: string; section: HTMLElement }[] = [];

  const update = (c: Conversation): void => {
    const parts = c.thread.kind === "path" ? pathParts(c) : directParts(c);
    const words = summaryWords(c);
    if (summary.textContent !== words) {
      summary.textContent = words;
    }
    const next = parts.map((part, i) => {
      const held = drawn[i];
      return held?.key === part.key ? held : { key: part.key, section: part.draw() };
    });
    for (const old of drawn) {
      if (!next.includes(old)) {
        old.section.remove();
      }
    }
    let after: Element = summary;
    for (const part of next) {
      if (part.section.previousElementSibling !== after) {
        after.after(part.section);
      }
      after = part.section;
    }
    drawn = next;
  };
  update(first);
  enhanceContextPanel(root, { signal });
  return { root, main, update };
}
