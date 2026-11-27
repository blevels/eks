package namespace

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentIdenticalLookupsQueryOnce(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return []Namespace{cacheRow()}, nil
	}), 4)
	var group sync.WaitGroup
	const workers = 32
	group.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer group.Done()
			found, err := c.Find(context.Background(), "id", "12")
			if err != nil || len(found) != 1 || found[0] != cacheRow() {
				t.Error(found, err)
				return
			}
			found[0].Name = "caller-owned"
		}()
	}
	<-started
	close(release)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate concurrent queries", calls.Load())
	}
}

func TestClearDuringQueryDoesNotRestoreOldSnapshot(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) {
		n := cacheRow()
		if calls.Add(1) == 1 {
			close(started)
			<-release
			n.Name = "before-clear"
		} else {
			n.Name = "after-clear"
		}
		return []Namespace{n}, nil
	}), 2)
	done := make(chan error, 1)
	go func() {
		found, err := c.Find(context.Background(), "id", "12")
		if err == nil && found[0].Name != "before-clear" {
			err = errors.New("unexpected in-flight snapshot")
		}
		done <- err
	}()
	<-started
	c.Clear()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		found, err := c.Find(context.Background(), "id", "12")
		if err != nil || found[0].Name != "after-clear" {
			t.Fatal("stale in-flight query repopulated cache", found, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
}

func TestWaitingMissCanCancelWhileHitsRemainAvailable(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c := makeCache(t, findFunc(func(_ context.Context, _, target string) ([]Namespace, error) {
		calls.Add(1)
		if target == "slow" {
			close(started)
			<-release
		}
		return []Namespace{cacheRow()}, nil
	}), 2)
	if _, err := c.Find(context.Background(), "name", "warm"); err != nil {
		t.Fatal(err)
	}
	slowDone := make(chan error, 1)
	go func() {
		_, err := c.Find(context.Background(), "name", "slow")
		slowDone <- err
	}()
	<-started
	// A hit must not wait for the slow miss, even on a different key.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Find(ctx, "name", "warm"); err != nil {
		t.Fatal("hit blocked behind query", err)
	}
	waitCtx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	found, err := c.Find(waitCtx, "name", "queued")
	if !errors.Is(err, context.DeadlineExceeded) || found != nil {
		t.Fatal(found, err)
	}
	if calls.Load() != 2 {
		t.Fatal("canceled waiter reached source", calls.Load())
	}
	close(release)
	if err := <-slowDone; err != nil {
		t.Fatal(err)
	}
}

func TestCanceledQueryDoesNotPoisonNextCaller(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	c := makeCache(t, findFunc(func(ctx context.Context, _, _ string) ([]Namespace, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []Namespace{cacheRow()}, nil
	}), 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Find(ctx, "id", "12"); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	found, err := c.Find(context.Background(), "id", "12")
	if err != nil || len(found) != 1 || calls.Load() != 2 {
		t.Fatal(found, err, calls.Load())
	}
}

func TestCacheConcurrentLookupsAndClear(t *testing.T) {
	c := makeCache(t, findFunc(func(context.Context, string, string) ([]Namespace, error) {
		return []Namespace{cacheRow()}, nil
	}), 3)
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for j := 0; j < 30; j++ {
				if (worker+j)%7 == 0 {
					c.Clear()
				}
				found, err := c.Find(context.Background(), "name", fmt.Sprint(j%5))
				if err != nil || len(found) != 1 || found[0] != cacheRow() {
					t.Error(found, err)
					return
				}
				found[0].Name = "caller-copy"
			}
		}(i)
	}
	group.Wait()
	if len(c.entries) > c.limit || c.recent.Len() != len(c.entries) {
		t.Fatal("cache bound or bookkeeping violated")
	}
}
