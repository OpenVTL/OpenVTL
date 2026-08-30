// Package sysexec wraps external tool invocation (mtx, lsscsi, sg_inq,
// zfs, zpool, targetcli, journalctl). Everything the daemon learns
// about the world outside /proc//sys comes through here, with timeouts.
package sysexec

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// Run executes a command with a timeout and returns stdout.
//
// WaitDelay matters: a SCSI probe against a stale mhVTL sg node goes
// D-state and never dies on SIGKILL — without it, cmd.Wait blocks
// forever PAST the context timeout and wedged the whole engine start
// during the v0.6 window-1 teardown. With WaitDelay the pipes are
// abandoned and Run returns ErrWaitDelay while the zombie stays
// behind (harmless; a reboot reaps it).
func Run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.WaitDelay = 5 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s: %w (stderr: %.200s)", name, err, errb.String())
	}
	return out.String(), nil
}
