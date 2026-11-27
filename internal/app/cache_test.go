package app

import (
	"context"
	"errors"
	"testing"

	"eks/internal/namespace"
)

func TestCachedNamespaceDoesNotCacheProductionAuthorization(t *testing.T) {
	a, conn, fc, ui := fixture()
	production(a)
	cache, err := namespace.NewCache(a.Store, 4)
	if err != nil {
		t.Fatal(err)
	}
	a.Store = cache
	if err := a.Run(context.Background()); err != nil || len(conn.requests) != 1 || fc.listCalls != 1 {
		t.Fatal(err, len(conn.requests), fc.listCalls)
	}
	// This second run reuses the namespace snapshot, but the service no longer
	// reports an active firecall. A cached database row must not authorize it.
	fc.active = nil
	ui.yes = false
	if err := a.Run(context.Background()); err == nil || len(conn.requests) != 1 || fc.listCalls != 2 || fc.created != 0 {
		t.Fatal("cached namespace bypassed live authorization", err, len(conn.requests), fc.listCalls, fc.created)
	}
	fc.err = errors.New("authorization service unavailable")
	if err := a.Run(context.Background()); err == nil || len(conn.requests) != 1 {
		t.Fatal("authorization failure used cached access", err)
	}
}

func TestCacheCannotHideInvalidNamespaceEnvironment(t *testing.T) {
	a, conn, fc, _ := fixture()
	s := a.Store.(fakeStore)
	s.items[0].Environment = "production"
	cache, err := namespace.NewCache(s, 2)
	if err != nil {
		t.Fatal(err)
	}
	a.Store = cache
	for i := 0; i < 2; i++ {
		if err := a.Run(context.Background()); err == nil || len(conn.requests) != 0 || fc.listCalls != 0 {
			t.Fatal("invalid environment passed cache validation", err)
		}
	}
}
