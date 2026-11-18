import { createIcons, type IconName } from "@cplieger/ui/icons";

const icons = createIcons({ base: __ICON_BASE__ });

/** A decorative icon from the bundle's icon folder, at the ui size. */
export function icon(name: IconName): HTMLElement {
  return icons.icon(name);
}
