package linode

import (
	"errors"
	"testing"
)

// IsNotFound lets callers make delete/cleanup operations idempotent: a resource
// that is already gone (the API returns 404 / "not found") means the goal is
// already achieved, so it should be treated as success rather than an error.
func TestIsNotFound(t *testing.T) {
	notFound := []error{
		// The real error the do() wrapper produces (from the reported bug).
		errors.New(`linode GET /object-storage/buckets/sg-sin-2/vmrep-audit-100228086/object-list?page_size=100: 404 Not Found: {"errors":[{"reason":"The specified bucket was not found."}]}`),
		errors.New("linode DELETE /volumes/123: 404 Not Found: {}"),
		errors.New("The specified bucket was not found."),
	}
	for _, err := range notFound {
		if !IsNotFound(err) {
			t.Errorf("IsNotFound should be true for %q", err)
		}
	}
	other := []error{
		nil,
		errors.New("linode POST /volumes: 400 Bad Request"),
		errors.New("linode GET /profile: 401 Unauthorized"),
		errors.New("linode GET /x: 409 Conflict: bucket not empty"),
		errors.New("dial tcp: connection refused"),
	}
	for _, err := range other {
		if IsNotFound(err) {
			t.Errorf("IsNotFound should be false for %v", err)
		}
	}
}
