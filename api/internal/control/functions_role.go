package control

// Only the reserved service-role encoding is eligible for preservation in the
// original branch Proxy route. Ordinary user roles still follow the catalog.
func reservedFunctionRole(name string) bool {
	if len(name) != 19 || name[:3] != "fn_" {
		return false
	}
	for _, c := range name[3:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
