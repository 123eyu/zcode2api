package shared

import (
	"context"
	"time"
)

func Pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(max(d, time.Millisecond))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func MaxTime(first, second time.Time) time.Time {
	if first.After(second) {
		return first
	}
	return second
}
