package native

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/configstore"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

// lossProxy retains TCP connections while silently discarding traffic in
// either direction. This models a network blackhole rather than a clean close.
type lossProxy struct {
	listener net.Listener
	target   string
	drop     *atomic.Bool
	dropped  atomic.Int64
	mu       sync.Mutex
	conns    []net.Conn
}

func newLossProxy(t *testing.T, target string, drop *atomic.Bool) *lossProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &lossProxy{listener: listener, target: target, drop: drop}
	go p.accept()
	t.Cleanup(func() {
		_ = listener.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, conn := range p.conns {
			_ = conn.Close()
		}
	})
	return p
}

func (p *lossProxy) accept() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.DialTimeout("tcp", p.target, 3*time.Second)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go p.forward(client, server)
		go p.forward(server, client)
	}
}

func (p *lossProxy) forward(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	var buf [32 * 1024]byte
	for {
		n, err := src.Read(buf[:])
		if n > 0 {
			if p.drop.Load() {
				p.dropped.Add(int64(n))
			} else if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// Keep the same official client alive while both HTTP and gRPC links silently
// lose packets. Its last verified revision must survive the outage, then
// manifest reconciliation must reach the newest revision after recovery.
func TestOfficialNacosClientConvergesAfterNetworkBlackhole(t *testing.T) {
	srv := nativeServer(t)
	store := configstore.NewGonacosStore(srv.Services().Config)
	var drop atomic.Bool
	httpProxy := newLossProxy(t, srv.HTTPAddr(), &drop)
	grpcProxy := newLossProxy(t, srv.GRPCAddr(), &drop)
	const namespace = "native-blackhole"
	client := nativeClientAt(t, httpProxy.listener.Addr().String(), grpcProxy.listener.Addr().String(), namespace)
	publish(t, store, namespace, 1, false)
	var consumer nativeConsumer
	// The official SDK establishes its gRPC connection asynchronously. Wait
	// for the initial verified snapshot before injecting the network fault.
	readyDeadline := time.Now().Add(10 * time.Second)
	for consumer.revision != 1 && time.Now().Before(readyDeadline) {
		consumer.refresh(client)
		if consumer.revision == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if consumer.lastErr != nil || consumer.revision != 1 || consumer.value("checkout") != false {
		t.Fatalf("initial state: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}
	if err := client.ListenConfig(vo.ConfigParam{DataId: configstore.ManifestDataID, Group: configstore.Group,
		OnChange: func(_, _, _, _ string) {}}); err != nil {
		t.Fatal(err)
	}

	drop.Store(true)
	publish(t, store, namespace, 2, true)
	publish(t, store, namespace, 3, true)
	deadline := time.Now().Add(5 * time.Second)
	for httpProxy.dropped.Load()+grpcProxy.dropped.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if httpProxy.dropped.Load()+grpcProxy.dropped.Load() == 0 {
		t.Fatal("network fault dropped no traffic; recovery test is inconclusive")
	}
	consumer.refresh(client)
	if consumer.revision != 1 || consumer.value("checkout") != false {
		t.Fatalf("unverified revision became visible during outage: revision=%d", consumer.revision)
	}

	drop.Store(false)
	deadline = time.Now().Add(30 * time.Second)
	for consumer.revision != 3 && time.Now().Before(deadline) {
		consumer.refresh(client)
		if consumer.revision == 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if consumer.lastErr != nil || consumer.revision != 3 || consumer.value("checkout") != true {
		t.Fatalf("same client did not converge after blackhole: revision=%d error=%v", consumer.revision, consumer.lastErr)
	}
}
