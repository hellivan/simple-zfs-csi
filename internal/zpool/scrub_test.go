package zpool

import (
	"context"
	"testing"
)

func TestParseScrubReport(t *testing.T) {
	clean := `  pool: tank
 state: ONLINE
  scan: scrub repaired 0B in 00:03:21 with 0 errors on Sun Jul 12 03:03:21 2026
config:
	NAME        STATE     READ WRITE CKSUM
	tank        ONLINE       0     0     0
errors: No known data errors
`
	if r := ParseScrubReport(clean); !r.OK {
		t.Errorf("clean scrub should be OK: %+v", r)
	}

	withErrors := `  pool: tank
 state: ONLINE
  scan: scrub repaired 0B in 00:03:21 with 3 errors on Sun Jul 12 03:03:21 2026
errors: 2 data errors, use '-v' for a list
`
	if r := ParseScrubReport(withErrors); r.OK {
		t.Errorf("scrub with errors must not be OK: %+v", r)
	}

	degraded := `  pool: tank
 state: DEGRADED
  scan: scrub repaired 0B in 00:03:21 with 0 errors on Sun Jul 12 03:03:21 2026
errors: No known data errors
`
	if r := ParseScrubReport(degraded); r.OK {
		t.Errorf("degraded pool must not be OK: %+v", r)
	}

	// A parse miss (no recognizable lines) must not fabricate a pass.
	if r := ParseScrubReport("garbage output"); r.OK {
		t.Errorf("unparseable status must not be OK: %+v", r)
	}
}

func TestPoolNameByGUID(t *testing.T) {
	run := func(_ context.Context, _ string, _ ...string) (string, error) {
		return "tank\t12140134988506841113\nbackup\t9999999999999999999\n", nil
	}
	name, err := PoolNameByGUID(context.Background(), run, "zpool", "9999999999999999999")
	if err != nil {
		t.Fatalf("PoolNameByGUID: %v", err)
	}
	if name != "backup" {
		t.Errorf("name = %q, want backup", name)
	}

	if _, err := PoolNameByGUID(context.Background(), run, "zpool", "nope"); err == nil {
		t.Errorf("expected error for unknown GUID")
	}
}
