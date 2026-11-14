import { el } from "@cplieger/reactive/dom";
import { enhanceListDetail } from "@cplieger/ui/list-detail";

import { icon } from "../icons.js";
import { routeHref, type Route } from "../router.js";
import type { ScreenMount } from "../shell.js";
import { getSettings } from "../wire/client.gen.js";
import type { ConversationCounts, Show } from "../wire/types.gen.js";
import { sendStore } from "./composer.js";
import { conversationDetail } from "./conversation.js";
import { filterGroup, filterSheet } from "./filters.js";
import { conversationList } from "./list.js";
import { newMessageDialog } from "./new-message.js";

type MessagesRoute = Extract<Route, { readonly screen: "messages" }>;

/**
 * Messages (shape 1): the Show filters in the rail, or in a sheet below
 * 60rem, and the conversation list beside the open conversation. Call it
 * once per page: the sends it keeps outlive each mount, so a send started
 * before a switch to another screen still shows on return.
 */
export function messagesScreen(): ScreenMount<"messages"> {
  const sends = sendStore();
  return (host, first, ctx) => {
    const { signal, router } = ctx;
    let route: MessagesRoute = first;
    let counts: ConversationCounts | null = null;

    const choose = (show: Show): void => {
      if (show !== route.show) {
        router.go({ ...route, show }, { replace: true });
      }
    };
    const rail = filterGroup("show-rail", choose);
    ctx.rail("Filters").append(rail.element);
    const sheet = filterSheet(choose);
    const setFilters = (): void => {
      rail.set(route.show, counts);
      sheet.set(route.show, counts);
    };

    const compose = newMessageDialog({
      signal,
      onSent: (posted) => {
        if (posted.thread === route.conversation) {
          detail.focus();
        } else {
          router.go({ ...route, conversation: posted.thread });
        }
      },
    });
    const retention = getSettings({ signal }).then((r) => (r.ok ? r.data.retention_ms : null));
    const detail = conversationDetail({
      signal,
      page: ctx.page,
      sends,
      live: ctx.live,
      retention,
      compose: (name) => {
        compose.open(name);
      },
    });
    const list = conversationList({
      signal,
      live: ctx.live,
      retention,
      href: (id) => routeHref({ ...route, conversation: id }),
      onCounts: (c) => {
        counts = c;
        setFilters();
      },
      onSettled: () => {
        if (route.conversation !== null) {
          detail.show(route.conversation, list.row(route.conversation));
        }
      },
      clearFilters: () => {
        choose("all");
      },
      newMessage: () => {
        compose.open();
      },
    });
    const newMessage = el(
      "button",
      {
        className: "cp-button",
        "data-variant": "primary",
        type: "button",
        "data-cp-tip": "New message",
      },
      icon("pen-line"),
      el("span", { className: "cp-hidden-narrow" }, "New message"),
    );
    newMessage.addEventListener(
      "click",
      () => {
        compose.open();
      },
      { signal },
    );

    const listDetail = el(
      "div",
      { className: "cp-list-detail", "data-variant": "wide" },
      list.element,
      detail.section,
    );
    host.append(
      el(
        "div",
        { className: "cp-heading-row" },
        el("h1", null, "Messages"),
        el("div", { className: "cp-toolbar" }, sheet.toggle, newMessage),
      ),
      listDetail,
      sheet.dialog,
      compose.dialog,
    );

    const open = enhanceListDetail(listDetail, {
      signal,
      onDismiss: () => {
        if (router.current.peek().cause === "push") {
          history.back();
        } else {
          router.go({ ...route, conversation: null });
        }
      },
    });

    const apply = (next: MessagesRoute, onLoad: boolean): void => {
      const reload = onLoad || next.show !== route.show;
      // open() moves focus to the heading, which a filter press must keep.
      const opened = onLoad || next.conversation !== route.conversation;
      route = next;
      setFilters();
      if (reload) {
        list.load(next.show, { announce: !onLoad });
      }
      list.select(next.conversation);
      if (next.conversation === null) {
        detail.clear();
        open.close();
        return;
      }
      detail.show(next.conversation, list.row(next.conversation));
      if (opened) {
        open.open(onLoad ? { still: true } : {});
      }
    };
    apply(first, true);

    return {
      update: (next) => {
        if (next.screen === "messages") {
          apply(next, false);
        }
      },
    };
  };
}
