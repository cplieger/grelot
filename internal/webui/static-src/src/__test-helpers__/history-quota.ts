import { beforeEach } from "vitest";

// WebKit refuses a page's 101st pushState or replaceState within 10 s,
// counted on the top page across every test file's frame (WebKit
// History::updateAndCheckStateObjectQuota). Each file records its writes on
// the top page, and a test starts once the last 10 s leave room for its own.
const span = 10_000;
const room = 70;

type Host = Window & { grelotHistoryWrites?: number[] };

function writes(): number[] {
  const host = (window.top ?? window) as Host;
  host.grelotHistoryWrites ??= [];
  return host.grelotHistoryWrites;
}

for (const method of ["pushState", "replaceState"] as const) {
  const write = Reflect.get(History.prototype, method) as History["pushState"];
  History.prototype[method] = function (this: History, ...args: Parameters<History["pushState"]>) {
    Reflect.apply(write, this, args);
    writes().push(Date.now());
  };
}

beforeEach(async () => {
  const seen = writes();
  for (;;) {
    const since = Date.now() - span;
    while (seen.length > 0 && (seen[0] ?? 0) <= since) {
      seen.shift();
    }
    if (seen.length <= room) {
      return;
    }
    await new Promise((done) => setTimeout(done, (seen[0] ?? since) - since + 1));
  }
});
