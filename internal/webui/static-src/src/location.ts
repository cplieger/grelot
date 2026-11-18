/** Leaves the app for url with a full page load, as a sign-in or sign-out needs. */
export function leaveTo(url: string): void {
  location.assign(url);
}
