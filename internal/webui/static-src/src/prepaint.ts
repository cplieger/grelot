// Loaded as a blocking script in <head>, so the first paint already has the
// stored theme and pointer tier (@cplieger/ui docs/design.md "Set up").
import { applyPointerTier } from "@cplieger/ui/pointer";
import { applyTheme } from "@cplieger/ui/theme";

import { pointerKey, themeKey } from "./storage-keys.js";

applyTheme({ storageKey: themeKey });
applyPointerTier({ storageKey: pointerKey });
