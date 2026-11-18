import { el } from "@cplieger/reactive/dom";
import { signal } from "@cplieger/reactive/signal";
import { installAnchors } from "@cplieger/ui/anchor";
import { installAnnouncer } from "@cplieger/ui/announce";
import { installDialogs } from "@cplieger/ui/dialog";
import { installPointerTier } from "@cplieger/ui/pointer";
import { installPress } from "@cplieger/ui/press";
import { installRadioFocus } from "@cplieger/ui/radio-focus";
import { createTheme } from "@cplieger/ui/theme";
import { installTooltips } from "@cplieger/ui/tooltip";

import { renderGate } from "./gate.js";
import { startLive } from "./live.js";
import { leaveTo } from "./location.js";
import { mountAgent } from "./messages/agent.js";
import { messagesScreen } from "./messages/screen.js";
import { parseRoute, signInHref, startRouter } from "./router.js";
import { mountSettings } from "./settings/screen.js";
import { startShell } from "./shell.js";
import { statisticsScreen } from "./stats/screen.js";
import { pointerKey, themeKey } from "./storage-keys.js";

const page = new AbortController().signal;

const resolvedTheme = signal<"light" | "dark">("light");
const theme = createTheme({
  storageKey: themeKey,
  onChange: (resolved) => {
    resolvedTheme.value = resolved;
  },
});
resolvedTheme.value = theme.resolved();
installPointerTier({ storageKey: pointerKey });
installPress();
installRadioFocus();
installAnnouncer({ signal: page });
installAnchors({ signal: page });
installTooltips({ signal: page });
installDialogs({ signal: page });

// The installers above append their tooltip and live regions to body, so the
// app draws into a root of its own: replacing body's children disconnects them.
const root = el("div");
document.body.prepend(root);

const { route } = parseRoute(location.pathname, location.search);
if (route.screen === "sign-in" || route.screen === "setup" || route.screen === "recover") {
  renderGate(root, route);
} else {
  const live = startLive({
    signal: page,
    onAuthLost: () => {
      const signIn = signInHref(location.pathname, location.search);
      if (signIn !== null) {
        leaveTo(signIn);
      }
    },
  });
  startShell({
    root,
    router: startRouter(page),
    live,
    theme,
    resolvedTheme,
    signal: page,
    screens: {
      messages: messagesScreen(),
      agent: mountAgent,
      statistics: statisticsScreen(),
      settings: mountSettings,
    },
  });
}
