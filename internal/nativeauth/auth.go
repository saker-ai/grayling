// Package nativeauth scopes embedded gonacos native SDK reads to one tenant.
package nativeauth

import (
	"strings"

	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/saker-ai/grayling/internal/platform"
)

// Authorize checks the SDK credential and the exact Nacos resource it may read.
func Authorize(store *platform.Store, secret, namespace, group, dataID string) error {
	if store == nil {
		return platform.ErrAccessDenied
	}
	tok, err := store.ResolveToken(secret)
	if err != nil || tok == nil || tok.Kind != platform.TokenSDK {
		return platform.ErrAccessDenied
	}
	if namespace == "" && group == "" && dataID == "" {
		return nil
	}
	if group != configstore.Group || dataID == "" ||
		(dataID != configstore.ManifestDataID &&
			!strings.HasPrefix(dataID, "golab.flag.") && !strings.HasPrefix(dataID, "golab.segment.")) {
		return platform.ErrAccessDenied
	}
	env, err := store.GetEnvironment(tok.WorkspaceID, tok.EnvironmentID)
	if err != nil || env == nil || env.NamespaceID != namespace {
		return platform.ErrAccessDenied
	}
	return nil
}
