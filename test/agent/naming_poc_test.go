//go:build integration

package agent_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/godeps/gonacos/pkg/naming"
	"github.com/godeps/gonacos/pkg/server"
)

// This probes the pinned backend, not the Agent authority or A09 in full.
func TestEmbeddedNamingDirectPOC(t *testing.T) {
	parallelAgentFixture(t)
	out, err := exec.CommandContext(t.Context(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/godeps/gonacos").Output()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(server.WithAddr("127.0.0.1:0"), server.WithGRPCAddr("127.0.0.1:0"), server.WithRoot(strings.TrimSpace(string(out))), server.WithDataDir(filepath.Join(t.TempDir(), "gonacos")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	s := srv.Services().Naming
	inst := naming.Instance{NamespaceID: "agent-poc", GroupName: "grayling", ServiceName: "checkout", ClusterName: "DEFAULT", IP: "10.20.0.10", Port: 8443, Weight: 1, Healthy: true, Enabled: true, Ephemeral: true, InstanceID: "agent-poc-instance", Metadata: map[string]string{"buildId": "build-a", "bootId": "boot-a"}}
	registered, err := s.RegisterInstance(inst)
	if err != nil {
		t.Fatal(err)
	}
	if registered.InstanceID != inst.InstanceID {
		t.Fatal("instance identity not retained")
	}
	list, err := s.ListInstances(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, true)
	if err != nil || len(list) != 1 || list[0].Metadata["bootId"] != "boot-a" {
		t.Fatalf("readback: %v %v", list, err)
	}
	sub := naming.Subscriber{NamespaceID: inst.NamespaceID, GroupName: inst.GroupName, ServiceName: inst.ServiceName, Addr: "10.20.0.11:9000", Agent: "grayling-agent-poc", ClientID: "test-connection"}
	if err := s.AddSubscriber(sub); err != nil {
		t.Fatal(err)
	}
	subs, err := s.ListSubscribers(inst.NamespaceID, inst.GroupName, inst.ServiceName, 1, 10)
	if err != nil || subs.Count != 1 {
		t.Fatalf("subscriber readback: %v %v", subs, err)
	}
	if err := s.Heartbeat(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, inst.IP, inst.Port, inst.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveSubscriber(inst.NamespaceID, inst.GroupName, inst.ServiceName, sub.Addr); err != nil {
		t.Fatal(err)
	}
	if err := s.DeregisterInstance(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, inst.IP, inst.Port, inst.InstanceID); err != nil {
		t.Fatal(err)
	}
	list, err = s.ListInstances(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, false)
	if err != nil || len(list) != 0 {
		t.Fatalf("deregister readback: %v %v", list, err)
	}
	if err := s.Heartbeat(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, inst.IP, inst.Port, inst.InstanceID); !errors.Is(err, naming.ErrInstanceNotFound) {
		t.Fatalf("deleted heartbeat: %v", err)
	}
}

func TestEmbeddedNamingLeaseSemanticsPOC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := naming.NewService()
		defer s.Stop()
		var notifications atomic.Int64
		s.SetPushFunc(func(_, _, _ string) { notifications.Add(1) })
		inst := naming.Instance{NamespaceID: "agent-poc", GroupName: "grayling", ServiceName: "checkout", ClusterName: "DEFAULT", IP: "10.20.0.10", Port: 8443, Healthy: true, Enabled: true, Ephemeral: true, InstanceID: "lease-poc"}
		if _, err := s.RegisterInstance(inst); err != nil {
			t.Fatal(err)
		}
		if naming.EphemeralLeaseInterval != 15*time.Second {
			t.Fatal("pinned backend lease default changed")
		}
		synctest.Wait()
		time.Sleep(10 * time.Second)
		if err := s.Heartbeat(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, inst.IP, inst.Port, inst.InstanceID); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Second)
		synctest.Wait()
		live, err := s.ListInstances(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, true)
		if err != nil || len(live) != 1 {
			t.Fatalf("heartbeat failed to renew: %v %v", live, err)
		}
		time.Sleep(11 * time.Second)
		synctest.Wait()
		live, err = s.ListInstances(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, true)
		if err != nil || len(live) != 0 {
			t.Fatalf("expired instance healthy: %v %v", live, err)
		}
		all, err := s.ListInstances(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, false)
		if err != nil || len(all) != 1 || all[0].Healthy {
			t.Fatalf("expiry should mark unhealthy, not delete: %v %v", all, err)
		}
		if notifications.Load() < 2 {
			t.Fatal("registration/expiry did not notify")
		}
		// Backend renewal has no Agent boot/lease/connection fencing. This successful
		// resurrection proves the Agent layer must enforce its own CAS before calls.
		if err := s.Heartbeat(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, inst.IP, inst.Port, inst.InstanceID); err != nil {
			t.Fatal(err)
		}
		live, err = s.ListInstances(inst.NamespaceID, inst.GroupName, inst.ServiceName, inst.ClusterName, true)
		if err != nil || len(live) != 1 {
			t.Fatalf("backend renewal semantics changed: %v %v", live, err)
		}
	})
}
