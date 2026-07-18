package zpool

import (
	"context"
	"fmt"
	"strings"
)

// PoolNameByGUID resolves a pool GUID to its current name on this node, or an
// error if no pool with that GUID is imported here.
func PoolNameByGUID(ctx context.Context, run Runner, zpoolBin, guid string) (string, error) {
	if zpoolBin == "" {
		zpoolBin = "zpool"
	}
	out, err := run(ctx, zpoolBin, "list", "-H", "-o", "name,guid")
	if err != nil {
		return "", fmt.Errorf("list pools: %w", err)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[1] == guid {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no pool with GUID %q is imported on this node", guid)
}

// Scrub starts a scrub of pool and blocks until it finishes (`zpool scrub -w`).
func Scrub(ctx context.Context, run Runner, zpoolBin, pool string) error {
	if zpoolBin == "" {
		zpoolBin = "zpool"
	}
	if _, err := run(ctx, zpoolBin, "scrub", "-w", pool); err != nil {
		return fmt.Errorf("scrub %s: %w", pool, err)
	}
	return nil
}

// Status returns the raw `zpool status <pool>` output.
func Status(ctx context.Context, run Runner, zpoolBin, pool string) (string, error) {
	if zpoolBin == "" {
		zpoolBin = "zpool"
	}
	out, err := run(ctx, zpoolBin, "status", pool)
	if err != nil {
		return "", fmt.Errorf("status %s: %w", pool, err)
	}
	return out, nil
}

// ScrubReport is the parsed outcome of a completed scrub, derived from
// `zpool status`. OK is true only when the pool is ONLINE, the last scan
// completed with zero errors, and there are no known data errors.
type ScrubReport struct {
	State  string
	Scan   string
	Errors string
	OK     bool
}

// ParseScrubReport interprets `zpool status <pool>` output into a pass/fail
// report. It is conservative: anything it cannot positively confirm as healthy
// is treated as not-OK, so a parse miss never fabricates a passing scrub.
func ParseScrubReport(status string) ScrubReport {
	var r ScrubReport
	for _, raw := range strings.Split(status, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(line, "state:"):
			r.State = strings.TrimSpace(strings.TrimPrefix(line, "state:"))
		case strings.HasPrefix(line, "scan:"):
			r.Scan = strings.TrimSpace(strings.TrimPrefix(line, "scan:"))
		case strings.HasPrefix(line, "errors:"):
			r.Errors = strings.TrimSpace(strings.TrimPrefix(line, "errors:"))
		}
	}

	stateOK := strings.EqualFold(r.State, "ONLINE")
	errorsOK := strings.Contains(strings.ToLower(r.Errors), "no known data errors")
	// The scan line reads e.g. "scrub repaired 0B in 00:03:21 with 0 errors on …".
	// A scan that reported >0 errors ("with 3 errors") is not OK; a line without a
	// "with N errors" clause (e.g. "none requested") does not by itself fail.
	scanOK := !strings.Contains(r.Scan, "with ") || strings.Contains(r.Scan, "with 0 errors")

	r.OK = stateOK && errorsOK && scanOK
	return r
}
