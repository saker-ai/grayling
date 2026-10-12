package ai

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/godeps/golab/internal/configstore"
	"github.com/godeps/golab/internal/nacos"
	"github.com/godeps/golab/internal/platform"
)

func TestExternalNacosConfigAndAI(t *testing.T) {
	var mu sync.Mutex
	namespaces := map[string]bool{}
	docs := map[string]string{}
	key := func(namespace, group, dataID string) string { return namespace + "\x00" + group + "\x00" + dataID }
	write := func(w http.ResponseWriter, code int, data any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": "success", "data": data})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("accessToken") != "test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/nacos/v3/admin/core/namespace/list":
			write(w, 0, []any{})
		case "/nacos/v3/admin/core/namespace/check":
			write(w, 0, namespaces[r.Form.Get("namespaceId")])
		case "/nacos/v3/admin/core/namespace":
			namespaces[r.Form.Get("namespaceId")] = true
			write(w, 0, true)
		case "/nacos/v3/admin/cs/config":
			k := key(r.Form.Get("namespaceId"), r.Form.Get("groupName"), r.Form.Get("dataId"))
			switch r.Method {
			case http.MethodGet:
				value, ok := docs[k]
				if !ok {
					write(w, 20004, nil)
					return
				}
				write(w, 0, map[string]string{"content": value})
			case http.MethodPost:
				docs[k] = r.Form.Get("content")
				write(w, 0, true)
			case http.MethodDelete:
				delete(docs, k)
				write(w, 0, true)
			default:
				t.Errorf("unexpected config method %s", r.Method)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client, err := nacos.NewClient(nacos.Config{URL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}
	flags := configstore.NewNacosStore(client)
	store, err := platform.NewStore("")
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := store.CreateWorkspace("acme", "")
	env, _ := store.CreateEnvironment(ws.ID, "prod", "")
	if _, err := flags.PublishDoc(env.NamespaceID, "flag.v1", []byte(`{"enabled":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := flags.GetDoc(env.NamespaceID, "flag.v1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := flags.GetDoc(env.NamespaceID, "missing"); !errors.Is(err, configstore.ErrNotFound) {
		t.Fatalf("missing config: %v", err)
	}

	aiStore := configstore.NewNacosAIStore(client)
	service := NewExternalService(store, aiStore)
	for _, typ := range []string{platform.AITypePrompt, platform.AITypeSkill, platform.AITypeAgentSpec} {
		ref, err := service.RegisterResource(ws.ID, env.ID, typ, "assistant", "Assistant", "owner")
		if err != nil {
			t.Fatal(err)
		}
		first, err := service.PublishVersion(ws.ID, env.ID, typ, "assistant", "first content", "owner")
		if err != nil {
			t.Fatal(err)
		}
		second, err := service.PublishVersion(ws.ID, env.ID, typ, "assistant", "second content", "owner")
		if err != nil || first.Version == second.Version {
			t.Fatalf("second version: %v, first=%v second=%v", err, first, second)
		}
		old, err := service.Resolve(ws.ID, env.ID, typ, "assistant", first.Version)
		if err != nil || old.Content != "first content" {
			t.Fatalf("resolve old version: %v, content=%v", err, old)
		}
		latest, err := service.Resolve(ws.ID, env.ID, typ, "assistant", "")
		if err != nil || latest.Content != "second content" {
			t.Fatalf("resolve latest: %v, content=%v", err, latest)
		}
		mu.Lock()
		docs[key(env.NamespaceID, configstore.AIGroup, externalDataID(typ, ref.PhysicalID, first.Version))] = "tampered"
		mu.Unlock()
		if _, err := service.Resolve(ws.ID, env.ID, typ, "assistant", first.Version); !errors.Is(err, ErrDigestDrift) {
			t.Fatalf("tampered version should fail closed: %v", err)
		}
	}
	if err := flags.DeleteDoc(env.NamespaceID, "flag.v1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := flags.GetDoc(env.NamespaceID, "flag.v1"); !errors.Is(err, configstore.ErrNotFound) {
		t.Fatalf("deleted config: %v", err)
	}
}
