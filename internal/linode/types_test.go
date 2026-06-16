package linode

import "testing"

func gib(n int64) int64 { return n * 1 << 30 }

var testTypes = []LinodeType{
	{ID: "g6-nanode-1", Class: "nanode", DiskMB: 25 * 1024},
	{ID: "g6-standard-1", Class: "standard", DiskMB: 50 * 1024},
	{ID: "g6-standard-2", Class: "standard", DiskMB: 80 * 1024},
	{ID: "g6-standard-4", Class: "standard", DiskMB: 160 * 1024},
	{ID: "g6-dedicated-2", Class: "dedicated", DiskMB: 80 * 1024},
}

func TestClosestType(t *testing.T) {
	cases := []struct {
		name   string
		group  string
		bytes  int64
		wantID string
		wantOK bool
	}{
		{"exact 80GB shared", "shared", gib(80), "g6-standard-2", true},
		{"75GB rounds up to 80GB", "shared", gib(75), "g6-standard-2", true},
		{"26GB picks next shared", "shared", gib(26), "g6-standard-1", true},
		{"80GB dedicated", "dedicated", gib(80), "g6-dedicated-2", true},
		{"too big for shared", "shared", gib(200), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ClosestType(testTypes, c.group, c.bytes)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got.ID != c.wantID {
				t.Fatalf("got %q, want %q", got.ID, c.wantID)
			}
		})
	}
}
