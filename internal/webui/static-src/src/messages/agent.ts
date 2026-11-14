import { el } from "@cplieger/reactive/dom";

import { ordinal } from "../format.js";
import { icon } from "../icons.js";
import type { ScreenMount } from "../shell.js";
import { failureWords, type ApiResult } from "../transport.js";
import { getAgent, getSettings } from "../wire/client.gen.js";
import type { Agent, AgentPage, Reservation } from "../wire/types.gen.js";
import { agentHistory } from "./history.js";
import { agentLink, isUser } from "./names.js";
import { newMessageDialog } from "./new-message.js";
import { relativeTime, retryButton } from "./parts.js";
import { reader, type Answer } from "./reader.js";

type Failure = Extract<ApiResult<unknown, string>, { readonly ok: false }>;

function stateWord(a: Agent): HTMLElement {
  return a.state === "live"
    ? el("span", { className: "cp-state", "data-tone": "success" }, icon("circle-check"), "Live")
    : el("span", { className: "cp-state", "data-tone": "neutral" }, icon("moon"), "Dormant");
}

/** The heading's state line: Live or Dormant, then Online behind its live dot. */
function stateLine(line: HTMLElement, a: Agent): void {
  line.replaceChildren(stateWord(a));
  if (a.online === true) {
    line.append(
      " ",
      el("span", { className: "cp-dot", "data-tone": "success", "data-live": "" }),
      " Online",
    );
  }
}

function machineWords(a: Agent): string | null {
  if (a.host === undefined || a.host === "") {
    return null;
  }
  return a.provider === undefined || a.provider === "" ? a.host : `${a.host} (${a.provider})`;
}

function monoList(values: readonly string[]): HTMLElement {
  return el("ul", null, ...values.map((v) => el("li", { className: "cp-mono", dir: "auto" }, v)));
}

/** The agent's card (shape 2): each fact it has, as .cp-facts. */
function cardFacts(page: AgentPage, signal: AbortSignal): HTMLElement {
  const a = page.agent;
  const dl = el("dl", { className: "cp-facts" });
  const fact = (label: string, value: Node | string | null): void => {
    if (value !== null) {
      dl.append(el("dt", null, label), el("dd", null, value));
    }
  };
  const at = (ms: number | undefined): HTMLElement | null =>
    ms === undefined ? null : relativeTime(ms, signal);
  fact("Description", a.description === "" ? null : el("bdi", null, a.description));
  const machine = machineWords(a);
  fact("Machine", machine === null ? null : el("bdi", null, machine));
  fact("Registered", at(a.registered_ms));
  fact("Last seen", at(a.last_seen_ms));
  if (a.state === "live") {
    fact("Lease ends", at(a.expires_ms));
  } else {
    fact("Dormant since", at(a.dormant_since_ms));
  }
  fact("Paths", a.paths.length === 0 ? null : monoList(a.paths));
  fact("Follows", a.follows.length === 0 ? null : monoList(a.follows));
  fact(
    "Delegates",
    page.delegates.length === 0
      ? null
      : el(
          "ul",
          { className: "cp-stack", "data-gap": "tight" },
          ...page.delegates.map((d) => el("li", null, agentLink(d))),
        ),
  );
  return dl;
}

function reservationState(r: Reservation): HTMLElement {
  return r.state === "held"
    ? el("span", { className: "cp-state", "data-tone": "success" }, icon("lock"), "Held")
    : el(
        "span",
        { className: "cp-state", "data-tone": "neutral" },
        icon("clock"),
        `Queued, ${ordinal(r.place ?? 1)}`,
      );
}

/** The agent's reservations as a .cp-table: Until and Reason leave first on a narrow box. */
function reservationsTable(rs: readonly Reservation[], signal: AbortSignal): HTMLElement {
  if (rs.length === 0) {
    return el("p", { className: "cp-meta" }, "Holds no reservations.");
  }
  const head = (words: string, priority?: string): HTMLElement =>
    el(
      "th",
      { scope: "col", ...(priority === undefined ? {} : { "data-priority": priority }) },
      words,
    );
  return el(
    "div",
    { className: "cp-panel", "data-flush": "" },
    el(
      "table",
      { className: "cp-table", "aria-labelledby": "agent-reservations" },
      el(
        "thead",
        null,
        el("tr", null, head("Path"), head("State"), head("Until", "mid"), head("Reason", "low")),
      ),
      el(
        "tbody",
        null,
        ...rs.map((r) =>
          el(
            "tr",
            null,
            el("th", { scope: "row" }, el("code", { className: "cp-mono", dir: "auto" }, r.path)),
            el("td", null, reservationState(r)),
            el("td", { "data-priority": "mid" }, relativeTime(r.expires_ms, signal)),
            el("td", { "data-priority": "low", dir: "auto" }, r.reason ?? ""),
          ),
        ),
      ),
    ),
  );
}

function notice(words: readonly (Node | string)[], retry: () => void): HTMLElement {
  return el(
    "div",
    { className: "cp-notice", "data-tone": "danger" },
    icon("circle-x"),
    el("p", null, ...words),
    retryButton(retry),
  );
}

function skeletonLines(n: number): HTMLElement[] {
  return Array.from({ length: n }, () => el("span", { className: "cp-skeleton" }));
}

/**
 * The agent page (shape 2): its heading with its state and "Message @name",
 * its card beside its reservations and history, kept current by the live
 * stream.
 */
export const mountAgent: ScreenMount<"agent"> = (host, route, ctx) => {
  const { signal, router, live } = ctx;
  const { name } = route;

  const compose = newMessageDialog({
    signal,
    onSent: (posted) => {
      router.go({ screen: "messages", show: "all", conversation: posted.thread });
    },
  });
  const retention = getSettings({ signal }).then((r) => (r.ok ? r.data.retention_ms : null));

  const state = el("p", { className: "cp-meta" });
  const write = el(
    "button",
    { className: "cp-button", type: "button", hidden: true },
    icon("pen-line"),
    `Message @${name}`,
  );
  write.addEventListener(
    "click",
    () => {
      compose.open(name);
    },
    { signal },
  );
  const content = el("div");
  host.append(
    el(
      "nav",
      { className: "cp-crumbs", "aria-label": "Breadcrumb" },
      el("a", { href: "/messages" }, "Messages"),
      el("span", { "aria-current": "page", dir: "auto" }, `@${name}`),
    ),
    el(
      "div",
      { className: "cp-heading-row" },
      el("div", null, el("h1", { dir: "auto" }, `@${name}`), state),
      el("div", { className: "cp-toolbar" }, write),
    ),
    content,
    compose.dialog,
  );

  const history = agentHistory({ name, signal, retention });
  let shown: {
    readonly panel: HTMLElement;
    readonly reservations: HTMLElement;
    readonly main: HTMLElement;
    key: { facts: string; reservations: string };
  } | null = null;
  let staleNotice: HTMLElement | null = null;

  const draw = (page: AgentPage): void => {
    stateLine(state, page.agent);
    write.hidden = isUser(name);
    const key = {
      facts: JSON.stringify([page.agent, page.delegates]),
      reservations: JSON.stringify(page.reservations),
    };
    if (shown === null) {
      const panel = el(
        "section",
        { className: "cp-panel", "aria-label": `About @${name}` },
        cardFacts(page, signal),
      );
      const reservations = el("div", null, reservationsTable(page.reservations, signal));
      const main = el(
        "div",
        { className: "cp-stack" },
        el("section", null, el("h2", { id: "agent-reservations" }, "Reservations"), reservations),
        history.section,
      );
      content.replaceChildren(el("div", { className: "cp-side-main" }, panel, main));
      shown = { panel, reservations, main, key };
      history.load();
      return;
    }
    if (shown.key.facts !== key.facts) {
      shown.panel.replaceChildren(cardFacts(page, signal));
    }
    if (shown.key.reservations !== key.reservations) {
      shown.reservations.replaceChildren(reservationsTable(page.reservations, signal));
    }
    shown.key = key;
  };

  const gone = (): void => {
    shown = null;
    state.replaceChildren();
    write.hidden = true;
    content.replaceChildren(
      el(
        "div",
        { className: "cp-empty" },
        el("strong", null, `No agent called @${name}.`),
        el(
          "p",
          null,
          "It may have retired or stayed dormant too long, or the address is mistyped.",
        ),
        el("a", { className: "cp-button", href: "/messages" }, "Back to messages"),
      ),
    );
  };

  const failed = (failure: Failure): void => {
    content.replaceChildren(
      notice(
        [el("bdi", null, `@${name}`), ` could not be loaded. ${failureWords(failure)}`],
        () => {
          void cards.retry();
        },
      ),
    );
  };

  const setStale = (failure: Failure | null): void => {
    staleNotice?.remove();
    staleNotice = null;
    if (failure !== null && shown !== null) {
      staleNotice = notice(
        [el("bdi", null, `@${name}`), ` could not be refreshed. ${failureWords(failure)}`],
        () => {
          void cards.retry();
        },
      );
      content.prepend(staleNotice);
    }
  };

  const fetchCard = async (asking: AbortSignal): Promise<Answer> => {
    const result = await getAgent({ name }, { signal: asking });
    if (result.ok) {
      return {
        draw: () => {
          content.removeAttribute("aria-busy");
          setStale(null);
          draw(result.data);
          return false;
        },
      };
    }
    if (result.code === "aborted") {
      return null;
    }
    if (result.code === "not_found") {
      return {
        draw: () => {
          content.removeAttribute("aria-busy");
          setStale(null);
          gone();
          return false;
        },
      };
    }
    return { failure: result };
  };

  const cards = reader(signal, {
    holds: () => shown !== null,
    load: fetchCard,
    refresh: fetchCard,
    skeleton: () => {
      content.setAttribute("aria-busy", "true");
      content.replaceChildren(
        el(
          "div",
          { className: "cp-side-main" },
          el("div", { className: "cp-panel" }, ...skeletonLines(4)),
          el("div", { className: "cp-stack" }, ...skeletonLines(3)),
        ),
      );
    },
    failed: (failure) => {
      content.removeAttribute("aria-busy");
      if (shown === null) {
        failed(failure);
      } else {
        setStale(failure);
      }
    },
  });

  live.watch(
    {
      changed: (change) => {
        if (change.agents) {
          void cards.changed();
        }
        if (change.conversations.length > 0) {
          void history.changed();
        }
      },
      resync: async (outer) => {
        await Promise.all([cards.resync(outer), history.resync(outer)]);
      },
    },
    signal,
  );
  void cards.load();
  return {};
};
