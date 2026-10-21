package control

import (
	"regexp"
	"strings"
)

var functionCSPOriginPattern = regexp.MustCompile(`^https?://[A-Za-z0-9.:\[\]-]+$`)

func functionConsoleCSP() string {
	policy := "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'"
	origin := functionPublicOrigin()
	if functionCSPOriginPattern.MatchString(origin) && validateFunctionPublicOrigin() == nil {
		policy = strings.Replace(policy, "connect-src 'self';", "connect-src 'self' "+origin+" "+strings.Replace(origin, "http", "ws", 1)+";", 1)
	}
	return policy
}
