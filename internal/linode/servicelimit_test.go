package linode

import (
	"errors"
	"testing"
)

// IsServiceLimit lets a cutover (or migration create) tell "the account is out
// of headroom for more active services" apart from any other 400 — a 400 has
// many unrelated causes (bad label, region mismatch, etc.) and must not be
// swallowed by matching on status alone. See F-30: this exact error was hit
// live during a two-disk cutover's final data-disk clone.
func TestIsServiceLimit(t *testing.T) {
	limited := []error{
		// The real error the do() wrapper produced, measured live (findings.md
		// F-30, the two-disk cutover's final clone).
		errors.New(`linode POST /volumes/17756356/clone: 400 Bad Request: {"errors": [{"reason": "You've reached a limit for the number of active services on your account. Please contact Support to request an increase and provide the total number of services you may need."}]}`),
		// A different endpoint hitting the same account-wide cap should still match.
		errors.New(`linode POST /linode/instances: 400 Bad Request: {"errors": [{"reason": "You have reached a limit for the number of active services on your account."}]}`),
	}
	for _, err := range limited {
		if !IsServiceLimit(err) {
			t.Errorf("IsServiceLimit should be true for %q", err)
		}
	}
	other := []error{
		nil,
		// A 400 is not, by itself, a service-limit hit — it has many other causes.
		errors.New(`linode POST /volumes: 400 Bad Request: {"errors": [{"reason": "Label must be 1-32 characters"}]}`),
		errors.New(`linode POST /volumes/1/clone: 400 Bad Request: {"errors": [{"reason": "The Linode's region does not match the requested region for creation"}]}`),
		errors.New("linode GET /linode/instances/42: 404 Not Found: {}"),
		errors.New(`linode GET /linode/instances/103154536: 403 Forbidden: {"errors":[{"reason":"You do not have permission to access this Linode"}]}`),
		errors.New("linode GET /profile: 401 Unauthorized"),
		errors.New("linode GET /x: 500 Internal Server Error: {}"),
		errors.New("dial tcp: connection refused"),
	}
	for _, err := range other {
		if IsServiceLimit(err) {
			t.Errorf("IsServiceLimit should be false for %v", err)
		}
	}
}
