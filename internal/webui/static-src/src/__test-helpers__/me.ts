import type { MeResponse } from "@cplieger/access";

/** GET /api/auth/me's answer for a password account named username. */
export function meAnswer(username: string, displayName?: string): MeResponse {
  return {
    user: {
      id: 1,
      username,
      ...(displayName === undefined ? {} : { display_name: displayName }),
      role: "admin",
      enabled: true,
      oidc_linked: false,
      created_at: "2026-11-13T09:00:00Z",
    },
    methods: {
      password: true,
      passkey_count: 0,
      oidc_linked: false,
      oidc_available: false,
      passkeys_available: true,
      password_login: true,
      locks: [],
    },
    can_link_oidc: false,
    sso_governed: false,
    session: {
      id: "s1",
      method: "password",
      auth_time: "2026-11-13T09:00:00Z",
      idle_deadline: "2026-11-14T09:00:00Z",
      absolute_deadline: "2026-12-13T09:00:00Z",
    },
  };
}
