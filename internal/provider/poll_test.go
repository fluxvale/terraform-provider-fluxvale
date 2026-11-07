package provider

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestPollReturnsOnFirstDone(t *testing.T) {
	calls := 0
	err := pollWithDeadline(context.Background(), time.Millisecond, time.Second, func(context.Context) (bool, error) {
		calls++
		return true, nil
	})
	if err != nil {
		t.Fatalf("pollWithDeadline: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (done on the first check)", calls)
	}
}

func TestPollTimesOut(t *testing.T) {
	err := pollWithDeadline(context.Background(), time.Millisecond, 20*time.Millisecond, func(context.Context) (bool, error) {
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("err = %v, want the timeout error naming the deadline", err)
	}
}

func TestPollFailsFastOnCheckError(t *testing.T) {
	boom := context.DeadlineExceeded
	err := pollWithDeadline(context.Background(), time.Millisecond, time.Second, func(context.Context) (bool, error) {
		return false, boom
	})
	if err != boom {
		t.Fatalf("err = %v, want the check error passed through", err)
	}
}

func TestPollRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := pollWithDeadline(ctx, time.Millisecond, time.Second, func(context.Context) (bool, error) {
		return false, nil
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
