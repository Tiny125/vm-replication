package linode

import (
	"errors"
	"testing"
)

// IsForbidden lets callers distinguish "the API definitively refused this" (403,
// e.g. the target Linode belongs to a different account than the token) from a
// transient or unrelated failure (network error, 5xx, a different 4xx). Callers
// that want to reject on a DEFINITE permission mismatch, but not on a flaky
// network blip, need this distinction — see F-26's appliance-account check.
func TestIsForbidden(t *testing.T) {
	forbidden := []error{
		// The real shape do() produces for a 403 (from the reported bug: a token
		// used against a Linode it doesn't own).
		errors.New(`linode GET /linode/instances/103154536: 403 Forbidden: {"errors":[{"reason":"You do not have permission to access this Linode","field":"linode_id"}]}`),
		errors.New("linode POST /volumes: 403 Forbidden: {}"),
	}
	for _, err := range forbidden {
		if !IsForbidden(err) {
			t.Errorf("IsForbidden should be true for %q", err)
		}
	}
	other := []error{
		nil,
		errors.New("linode GET /linode/instances/42: 404 Not Found: {}"),
		errors.New("linode GET /profile: 401 Unauthorized"),
		errors.New("linode GET /x: 500 Internal Server Error: {}"),
		errors.New("linode GET /x: 503 Service Unavailable"),
		errors.New("dial tcp: connection refused"),
		errors.New("context deadline exceeded"),
	}
	for _, err := range other {
		if IsForbidden(err) {
			t.Errorf("IsForbidden should be false for %v", err)
		}
	}
}
