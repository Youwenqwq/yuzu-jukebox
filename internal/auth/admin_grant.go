package auth

import "log"

// LogAdminGrant records authentication paths that grant an administrator role.
// The standard logger prefix supplies the grant time.
func LogAdminGrant(id Identity, via, remote string) {
	if !id.HasRole(RoleRoomAdmin) && !id.HasRole(RoleMediaAdmin) && !id.HasRole(RoleSysAdmin) {
		return
	}
	log.Printf("admin grant: name=%q id=%s via=%s remote=%s", id.Name, id.ID, via, remote)
}
