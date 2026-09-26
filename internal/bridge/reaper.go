package bridge

import (
	"context"
	"time"
)

// Reaping policy for layer 1 (the persistent pwsh processes).
const (
	// ReaperIntervalSec is how often the reaper sweeps.
	ReaperIntervalSec = 60

	// DefaultShellIdleSec stops an idle shell after this long without any
	// message in that chat. Long enough that a normal working session never
	// trips it, short enough that an overnight session does not hold a process
	// all night.
	DefaultShellIdleSec = 30 * 60
)

// reaper stops idle shells on a fixed interval.
//
// It holds no state of its own; the bridge supplies collectIdleShells, which
// runs under Bridge.mu, and stopIdle, which does the teardown. Keeping the
// teardown on the bridge side means the reaper never touches the maps it does
// not own.
type reaper struct {
	idle time.Duration
	logf func(format string, args ...any)
}

func newReaper(idleSec int, logf func(string, ...any)) *reaper {
	if idleSec <= 0 {
		idleSec = DefaultShellIdleSec
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &reaper{idle: time.Duration(idleSec) * time.Second, logf: logf}
}

// cutoff is the idle threshold, read by collectIdleShells.
func (r *reaper) cutoff() time.Duration { return r.idle }

// Run sweeps until ctx is cancelled.
func (r *reaper) Run(ctx context.Context, collect func() []string, stop func(context.Context, []string)) {
	r.logf("idle shell reaper: stopping shells idle over %s, every %s",
		r.idle, time.Duration(ReaperIntervalSec)*time.Second)

	t := time.NewTicker(time.Duration(ReaperIntervalSec) * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			keys := collect()
			if len(keys) == 0 {
				continue
			}
			r.logf("reaper: %d shell(s) idle over %s, stopping", len(keys), r.idle)
			stop(ctx, keys)
		}
	}
}

// shortKey trims a shell key for log lines.
func shortKey(k string) string {
	if len(k) > 24 {
		return k[:24] + "…"
	}
	return k
}
