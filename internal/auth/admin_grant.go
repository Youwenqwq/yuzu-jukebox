package auth

import "log"

// LogAdminGrant records authentication paths that grant either administrator role.
// The standard logger prefix supplies the grant time.
func LogAdminGrant(id Identity, via, remote string) {
	if !id.HasRole(RoleRoomAdmin) && !id.HasRole(RoleMediaAdmin) {
		return
	}
	log.Printf("admin grant: name=%q id=%s via=%s remote=%s", id.Name, id.ID, via, remote)
}
