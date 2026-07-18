// Command zpool-scrub runs a single ZFS pool scrub and exits with a status that
// reflects the result: 0 for a clean scrub, non-zero when the pool is unhealthy
// or the scan reported errors. It is launched by the operator-reconciled scrub
// CronJob (see design-decisions ADR-0012) on the node currently hosting the
// pool, and reuses the discovery plane's host-exec so it runs the host's own
// version-matched zpool (chroot/nsenter), important on Talos.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

func main() {
	var (
		poolGUID     string
		zpoolBin     string
		hostExecMode string
		hostRoot     string
		nsenterPID   int
		timeout      time.Duration
	)
	flag.StringVar(&poolGUID, "pool-guid", "", "GUID of the pool to scrub (required).")
	flag.StringVar(&zpoolBin, "zpool-bin", "zpool", "zpool binary name/path (resolved on the host when host-exec is on).")
	flag.StringVar(&hostExecMode, "host-exec-mode", "", "How to run the host's zpool: \"chroot\", \"nsenter\", or empty for the in-image tool.")
	flag.StringVar(&hostRoot, "host-root", "/host", "Container-visible mount of the host root filesystem (chroot mode).")
	flag.IntVar(&nsenterPID, "nsenter-target-pid", 1, "Host PID whose namespaces are entered (nsenter mode).")
	flag.DurationVar(&timeout, "timeout", 24*time.Hour, "Maximum time to wait for the scrub to complete.")
	flag.Parse()

	if poolGUID == "" {
		fmt.Fprintln(os.Stderr, "error: --pool-guid is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	run := zpool.HostExec{Mode: hostExecMode, HostRoot: hostRoot, TargetPID: nsenterPID}.BuildRunner(nil)

	pool, err := zpool.PoolNameByGUID(ctx, run, zpoolBin, poolGUID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve pool: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("scrubbing pool %q (guid %s)…\n", pool, poolGUID)

	if err := zpool.Scrub(ctx, run, zpoolBin, pool); err != nil {
		fmt.Fprintf(os.Stderr, "scrub failed: %v\n", err)
		os.Exit(1)
	}

	status, err := zpool.Status(ctx, run, zpoolBin, pool)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read status: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(status)

	report := zpool.ParseScrubReport(status)
	if !report.OK {
		fmt.Fprintf(os.Stderr, "scrub completed with problems: state=%q scan=%q errors=%q\n", report.State, report.Scan, report.Errors)
		os.Exit(1)
	}
	fmt.Printf("scrub of %q completed cleanly (%s)\n", pool, report.Scan)
}
