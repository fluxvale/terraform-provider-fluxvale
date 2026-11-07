package provider

import (
	"context"
	"fmt"
	"time"
)

// pollWithDeadline polls until check reports done, fails fast on a
// check error, or times out. The deploy and the teardown are both
// async Oban jobs (instance.ex) — create-wait and delete-wait ride
// this loop with a generous deadline and a non-blocking interval.
func pollWithDeadline(ctx context.Context, interval, timeout time.Duration, check func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		if time.Now().Add(interval).After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
