/** The localStorage keys the pre-paint script and the app share. */
export const themeKey = "grelot.theme";
export const pointerKey = "grelot.pointer";

/** The sessionStorage key of conversation id's unsent draft. */
export function draftKey(id: number): string {
  return `grelot.draft.${String(id)}`;
}
