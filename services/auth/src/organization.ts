export const organizationActions: Record<string,"GET"|"POST"> = {
 "organization/create":"POST", "organization/check-slug":"POST",
 "organization/update":"POST", "organization/delete":"POST",
 "organization/set-active":"POST", "organization/get-organization":"GET",
 "organization/get-full-organization":"GET", "organization/list":"GET",
 "organization/invite-member":"POST", "organization/cancel-invitation":"POST",
 "organization/accept-invitation":"POST", "organization/reject-invitation":"POST",
 "organization/get-invitation":"GET", "organization/list-invitations":"GET",
 "organization/list-user-invitations":"GET", "organization/remove-member":"POST",
 "organization/update-member-role":"POST", "organization/leave":"POST",
 "organization/get-active-member":"GET", "organization/get-active-member-role":"GET",
 "organization/list-members":"GET", "organization/has-permission":"POST",
};
