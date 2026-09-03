package linode

import "strings"

// IsForbidden reports whether err indicates the API definitively refused the
// request with HTTP 403. It lets callers act on a DEFINITE permission/account
// mismatch (e.g. a token used against a Linode owned by a different account)
// while leaving transient or unrelated failures (network errors, 5xx, other
// 4xx) alone. The do() wrapper formats errors as
// "linode <METHOD> <path>: <status>: <body>", so we match the 403 status the
// same way notfound.go matches 404.
func IsForbidden(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "403 forbidden")
}
