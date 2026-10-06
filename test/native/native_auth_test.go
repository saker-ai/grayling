package native

import (
	json "encoding/json/v2"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/pkg/model"
	"github.com/godeps/gonacos/pkg/server"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

func TestOfficialNacosClientNativeConfigScope(t *testing.T) {
	srv := nativeServer(t, server.WithNativeConfigAuth(func(secret, tenant, group, dataID string) error {
		if secret != "sdk-a" && secret != "sdk-b" {
			return errors.New("unknown credential")
		}
		if tenant == "" && group == "" && dataID == "" {
			return nil
		}
		if tenant != "tenant-"+string(secret[len(secret)-1]) || group != configstore.Group || dataID == "" {
			return errors.New("wrong scope")
		}
		return nil
	}))
	store := configstore.NewGonacosStore(srv.Services().Config)
	publish(t, store, "tenant-a", 1, true)
	clientA := nativeClientAtWithOptions(t, srv.HTTPAddr(), srv.GRPCAddr(), "tenant-a",
		constant.WithUsername("golab-sdk"), constant.WithPassword("sdk-a"))
	if _, revision, err := nativeSnapshot(clientA); err != nil || revision != 1 {
		t.Fatalf("authorized snapshot revision=%d err=%v", revision, err)
	}
	clientB := nativeClientAtWithOptions(t, srv.HTTPAddr(), srv.GRPCAddr(), "tenant-a",
		constant.WithUsername("golab-sdk"), constant.WithPassword("sdk-b"))
	if _, err := clientB.GetConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group}); err == nil {
		t.Fatal("credential B read tenant A manifest")
	}
	if _, err := clientB.GetConfig(vo.ConfigParam{DataId: configstore.FlagDataID("checkout", 1), Group: configstore.Group}); err == nil {
		t.Fatal("credential B read tenant A immutable document")
	}
	changes := make(chan string, 1)
	_ = clientB.ListenConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group,
		OnChange: func(_, _, _, content string) { changes <- content }})
	publish(t, store, "tenant-a", 2, false)
	select {
	case <-changes:
		t.Fatal("credential B received tenant A subscription update")
	case <-time.After(time.Second):
	}
	resp, err := http.Get("http://" + srv.HTTPAddr() + "/nacos/v1/cs/configs?tenant=tenant-a&group=GOLAB_FLAGS&dataId=golab.manifest")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("HTTP bypass status=%d, want 404", resp.StatusCode)
	}
}

func TestOfficialNacosClientRevocationStopsExistingPush(t *testing.T) {
	var revoked atomic.Bool
	srv := nativeServer(t, server.WithNativeConfigAuth(func(secret, tenant, group, dataID string) error {
		if secret != "sdk-a" || revoked.Load() {
			return errors.New("credential revoked")
		}
		if tenant == "" && group == "" && dataID == "" {
			return nil
		}
		if tenant != "tenant-a" || group != configstore.Group || dataID != configstore.ManifestDataID {
			return errors.New("wrong scope")
		}
		return nil
	}))
	store := configstore.NewGonacosStore(srv.Services().Config)
	publish(t, store, "tenant-a", 1, true)
	client := nativeClientAtWithOptions(t, srv.HTTPAddr(), srv.GRPCAddr(), "tenant-a",
		constant.WithUsername("golab-sdk"), constant.WithPassword("sdk-a"))
	changes := make(chan int64, 8)
	if err := client.ListenConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group,
		OnChange: func(_, _, _, content string) {
			var manifest model.Manifest
			if json.Unmarshal([]byte(content), &manifest) == nil {
				select {
				case changes <- manifest.Revision:
				default:
				}
			}
		}}); err != nil {
		t.Fatal(err)
	}
	publish(t, store, "tenant-a", 2, false)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case revision := <-changes:
			if revision == 2 {
				goto subscribed
			}
		case <-deadline:
			t.Fatal("authorized subscription did not receive revision 2")
		}
	}
subscribed:
	revoked.Store(true)
	// The official SDK may return a cached last-known-good config after a
	// failed network read, so GetConfig alone cannot prove server access.
	publish(t, store, "tenant-a", 3, true)
	quiet := time.After(time.Second)
	for {
		select {
		case revision := <-changes:
			if revision == 3 {
				t.Fatal("revoked credential received push")
			}
		case <-quiet:
			return
		}
	}
}
