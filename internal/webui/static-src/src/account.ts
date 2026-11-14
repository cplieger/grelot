import { createAuthClient } from "@cplieger/access";
import { createFetch, type ApiErr } from "@cplieger/reactive/fetch";

import { leaveTo } from "./location.js";
import { signInHref } from "./router.js";
import type { ApiResult } from "./transport.js";

/** The signed-in account, as the header's account menu names it. */
export interface Account {
  readonly username: string;
  readonly displayName: string | null;
}

// accesshttp's routes under the prefixes internal/auth mounts them at. A 401
// sends the browser to sign-in, as transport.ts does for grelot's own routes.
const auth = createAuthClient({
  fetch: createFetch({
    credentials: "same-origin",
    onError: (err) => {
      if (err.status !== 401) {
        return;
      }
      const signIn = signInHref(location.pathname, location.search);
      if (signIn !== null) {
        leaveTo(signIn);
      }
    },
  }),
  prefix: "/api/auth",
  oidcPrefix: "/api/auth/oidc",
});

// reactive's reserved codes for a request that got no answer (status 0).
const unanswered = new Map<string, "deadline" | "aborted">([
  ["timeout", "deadline"],
  ["cancelled", "aborted"],
]);

function codeOf(err: ApiErr): "http" | "decode" | "network" | "deadline" | "aborted" {
  if (err.status === 0) {
    return unanswered.get(err.code ?? "") ?? "network";
  }
  return err.status < 300 ? "decode" : "http";
}

/** err as transport.ts's failure: the server's own words, or null when it sent none. */
function failed(err: ApiErr): ApiResult<never> {
  const body = err.body;
  const words =
    typeof body === "object" && body !== null && Object.hasOwn(body, "error")
      ? (body as Record<string, unknown>)["error"]
      : null;
  return {
    ok: false,
    status: err.status,
    code: codeOf(err),
    message: typeof words === "string" && words !== "" ? words : null,
  };
}

export async function fetchAccount(): Promise<ApiResult<Account>> {
  const res = await auth.me();
  if (!res.ok) {
    return failed(res);
  }
  const { username, display_name: display } = res.data.user;
  return {
    ok: true,
    data: { username, displayName: display === undefined || display === "" ? null : display },
  };
}

/** Ends this browser's session. */
export async function signOut(): Promise<ApiResult<undefined>> {
  const res = await auth.logout();
  return res.ok ? { ok: true, data: undefined } : failed(res);
}
