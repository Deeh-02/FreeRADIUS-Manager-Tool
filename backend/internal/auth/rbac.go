package auth

// Role constants
const (
	RoleSuperAdmin = "super_admin"
	RoleAdmin      = "admin"
	RoleOperator   = "operator"
)

// roleHierarchy maps each role to its numeric level (higher = more permissions).
var roleHierarchy = map[string]int{
	RoleOperator:   1,
	RoleAdmin:      2,
	RoleSuperAdmin: 3,
}

// HasRole returns true if the user's role is in the allowed roles list.
func HasRole(userRole string, allowedRoles ...string) bool {
	for _, r := range allowedRoles {
		if userRole == r {
			return true
		}
	}
	return false
}

// CanManageRole returns true if the actor can manage (create/edit/delete) the target role.
// Strictly greater, so even a super_admin cannot edit or delete another super_admin.
// Route access itself is decided by middleware.RequireRole with explicit role lists;
// roleHierarchy is only used here.
// A user cannot manage peers or higher roles.
func CanManageRole(actorRole, targetRole string) bool {
	return roleHierarchy[actorRole] > roleHierarchy[targetRole]
}
