package linode

import "testing"

// Object operations (presigned object-url, object-list, delete) use the bucket's
// storage-cluster id as the API path segment — which is NOT always the region.
// Singapore is the canonical case: a bucket whose REGION is "sg-sin-2" lives on
// the "sg-sin-1" object-storage cluster (hostname <label>.sg-sin-1.linodeobjects.com).
// If we build object URLs from the region instead of the cluster, the API returns
// 404 "The specified bucket was not found." for a bucket that plainly exists.
// pathSeg must therefore prefer Cluster, falling back to Region only when the
// cluster is unknown.
func TestBucketPathSegPrefersCluster(t *testing.T) {
	// Region ≠ cluster (the real SG mismatch): must use the cluster.
	b := Bucket{Label: "vmrep-audit-100668062", Region: "sg-sin-2", Cluster: "sg-sin-1"}
	if got := b.pathSeg(); got != "sg-sin-1" {
		t.Errorf("pathSeg() = %q, want the cluster %q", got, "sg-sin-1")
	}
	// No cluster known: fall back to the region.
	b = Bucket{Label: "x", Region: "us-ord"}
	if got := b.pathSeg(); got != "us-ord" {
		t.Errorf("pathSeg() with no cluster = %q, want the region %q", got, "us-ord")
	}
}
