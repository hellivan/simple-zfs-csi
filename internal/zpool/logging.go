package zpool

import (
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

// LoggingRunner wraps a Runner so every host command it executes — and its
// outcome (duration, trimmed output or error) — is logged at debug verbosity
// (V(1)). Command logging is therefore opt-in: enable it with
// --zap-log-level=debug and nothing is emitted at the default level. This keeps
// the logs quiet by default, which matters because several ZFS calls fail by
// design (e.g. the `zfs get` existence probe returning ErrNotExist before a
// create); logging every failure at error level would be misleading noise.
//
// To capture the fully resolved host command — including any chroot/nsenter
// prefix and the version-matched host binary path that HostExec adds — pass a
// LoggingRunner as the base to HostExec.BuildRunner:
//
//	HostExec{...}.BuildRunner(LoggingRunner(nil, log))
//
// A nil base uses the default exec runner.
func LoggingRunner(base Runner, log logr.Logger) Runner {
	if base == nil {
		base = execRunner
	}
	return func(ctx context.Context, name string, args ...string) (string, error) {
		cmd := name
		if len(args) > 0 {
			cmd = name + " " + strings.Join(args, " ")
		}
		start := time.Now()
		out, err := base(ctx, name, args...)
		dur := time.Since(start)
		if err != nil {
			log.V(1).Info("host command failed", "cmd", cmd, "duration", dur.String(), "err", err.Error())
			return out, err
		}
		log.V(1).Info("host command ran", "cmd", cmd, "duration", dur.String(), "output", strings.TrimSpace(out))
		return out, nil
	}
}
