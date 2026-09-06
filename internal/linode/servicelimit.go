package linode

import "strings"

// IsServiceLimit reports whether err is the Linode account-wide "active
// services" cap: a definite, distinctive failure ("You've reached a limit for
// the number of active services on your account...") as opposed to any other
// HTTP 400. A 400 has many unrelated causes (bad label, region mismatch,
// invalid size, ...), so — unlike IsNotFound/IsForbidden, which key off their
// status code alone — this matches the DISTINCTIVE phrasing Linode uses for
// this specific cap, not the 400 status by itself. The do() wrapper formats
// errors as "linode <METHOD> <path>: <status>: <body>", so, as with
// notfound.go and forbidden.go, a case-insensitive substring match on the
// formatted error is enough. Measured live (findings.md F-30): a two-disk
// cutover's final data-disk clone died with exactly this reason.
func IsServiceLimit(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "limit for the number of active services")
}
