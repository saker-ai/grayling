package main

import (
	"github.com/godeps/golab/internal/nativeauth"
	"github.com/godeps/golab/internal/platform"
)

// authorizeNativeConfig derives the only readable Nacos namespace from the
// SDK credential. It is called on login, every config query/subscription, and
// each push, so revocation also affects already established subscriptions.
func authorizeNativeConfig(store *platform.Store, secret, namespace, group, dataID string) error {
	return nativeauth.Authorize(store, secret, namespace, group, dataID)
}
