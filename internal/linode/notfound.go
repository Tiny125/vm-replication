package linode

import "strings"

// IsNotFound reports whether err indicates the target resource does not exist
// (the API returned HTTP 404, or an object-storage "not found" message). It lets
// callers make delete/cleanup idempotent: a resource that is already gone means
// the operation's goal is already met, so it should be treated as success rather
// than a hard error. The do() wrapper formats errors as
// "linode <METHOD> <path>: <status>: <body>", so we match the 404 status and the
// documented not-found phrasings.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404 not found") ||
		strings.Contains(msg, "not found.") ||
		strings.Contains(msg, "was not found")
}
