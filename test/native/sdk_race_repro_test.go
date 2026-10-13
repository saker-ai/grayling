//go:build sdk_race_repro

package native

import (
	"sync"
	"testing"

	"github.com/godeps/grayling/internal/configstore"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

// This opt-in reproducer isolates the official SDK's global rpc.clientMap
// shutdown/create race from GoLab evaluation and authorization. It is expected
// to fail under -race on v2.3.5; never use its failure as a passing CI gate.
func TestOfficialSDKClientMapRaceReproducer(t *testing.T) {
	srv := nativeServer(t)
	store := configstore.NewGonacosStore(srv.Services().Config)
	publish(t, store, "sdk-race", 1, true)
	for range 20 {
		closing := nativeClient(t, srv, "sdk-race")
		_, _ = closing.GetConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group})
		opening := nativeClient(t, srv, "sdk-race")
		var wg sync.WaitGroup
		wg.Go(closing.CloseClient)
		wg.Go(func() {
			_, _ = opening.GetConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group})
		})
		wg.Wait()
	}
}
