// Exact maintained Better Auth 1.7.7 methods. No caller chooses a plugin route.
export const adminActions: Record<string,"GET"|"POST"> = {
  "admin/get-user":"GET", "admin/list-users":"GET",
  "admin/create-user":"POST", "admin/update-user":"POST",
  "admin/set-role":"POST", "admin/ban-user":"POST", "admin/unban-user":"POST",
  "admin/list-user-sessions":"POST", "admin/revoke-user-session":"POST",
  "admin/revoke-user-sessions":"POST", "admin/set-user-password":"POST",
  "admin/remove-user":"POST", "admin/impersonate-user":"POST",
  "admin/stop-impersonating":"POST", "admin/has-permission":"POST",
};
