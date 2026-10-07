// golab-native-proxy terminates TLS while preserving one backend TCP connection
// for each accepted client connection. The backend must remain on loopback.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var nextConnection atomic.Uint64

type activeConnections struct {
	sync.Mutex
	closing bool
	conns   map[net.Conn]struct{}
}

func (a *activeConnections) add(conn net.Conn) bool {
	a.Lock()
	defer a.Unlock()
	if a.closing {
		_ = conn.Close()
		return false
	}
	a.conns[conn] = struct{}{}
	return true
}

func (a *activeConnections) remove(conn net.Conn) {
	a.Lock()
	delete(a.conns, conn)
	a.Unlock()
}

func (a *activeConnections) closeAll() {
	a.Lock()
	a.closing = true
	for conn := range a.conns {
		_ = conn.Close()
	}
	a.Unlock()
}

func main() {
	httpListen := flag.String("http-listen", ":8848", "TLS HTTP listener")
	grpcListen := flag.String("grpc-listen", ":9848", "TLS gRPC listener")
	httpBackend := flag.String("http-backend", "127.0.0.1:18848", "loopback HTTP backend")
	grpcBackend := flag.String("grpc-backend", "127.0.0.1:19848", "loopback gRPC backend")
	certFile := flag.String("cert", "/tls/tls.crt", "TLS certificate")
	keyFile := flag.String("key", "/tls/tls.key", "TLS private key")
	maxConnections := flag.Int("max-connections", 4096, "maximum simultaneous TLS client connections")
	flag.Parse()
	if *maxConnections <= 0 {
		log.Fatal("max-connections must be positive")
	}
	for _, backend := range []string{*httpBackend, *grpcBackend} {
		if !loopbackBackend(backend) {
			log.Fatalf("backend %q must use a numeric loopback address", backend)
		}
	}
	if *httpListen == *grpcListen || *httpBackend == *grpcBackend {
		log.Fatal("HTTP and gRPC require distinct listeners and backends")
	}
	certificate, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	active := &activeConnections{conns: make(map[net.Conn]struct{})}
	slots := make(chan struct{}, *maxConnections)
	var listeners []net.Listener
	var acceptWG, connectionWG sync.WaitGroup
	for _, endpoint := range []struct{ name, address, backend, alpn string }{
		{"http", *httpListen, *httpBackend, "http/1.1"},
		{"grpc", *grpcListen, *grpcBackend, "h2"},
	} {
		listener, listenErr := net.Listen("tcp", endpoint.address)
		if listenErr != nil {
			for _, previous := range listeners {
				_ = previous.Close()
			}
			acceptWG.Wait()
			log.Fatal(listenErr)
		}
		listeners = append(listeners, listener)
		tlsListener := tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{endpoint.alpn},
		})
		acceptWG.Go(func() {
			serve(ctx, tlsListener, endpoint.name, endpoint.backend, endpoint.alpn, active, slots, &connectionWG)
		})
	}
	log.Printf("ready http=%s grpc=%s", *httpListen, *grpcListen)
	<-ctx.Done()
	for _, listener := range listeners {
		_ = listener.Close()
	}
	acceptWG.Wait()
	active.closeAll()
	connectionWG.Wait()
}

func loopbackBackend(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func serve(ctx context.Context, listener net.Listener, name, backend, alpn string, active *activeConnections, slots chan struct{}, wg *sync.WaitGroup) {
	for {
		front, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("accept %s: %v", name, err)
			}
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			log.Printf("reject %s: connection limit reached", name)
			_ = front.Close()
			continue
		}
		if !active.add(front) {
			<-slots
			return
		}
		wg.Go(func() {
			defer func() { <-slots }()
			proxy(ctx, front, name, backend, alpn, active)
		})
	}
}

func proxy(ctx context.Context, front net.Conn, name, backend, alpn string, active *activeConnections) {
	defer front.Close()
	defer active.remove(front)
	id := nextConnection.Add(1)
	if err := front.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Printf("connection=%d handshake deadline: %v", id, err)
		return
	}
	tlsFront := front.(*tls.Conn)
	if err := tlsFront.Handshake(); err != nil {
		log.Printf("connection=%d handshake: %v", id, err)
		return
	}
	negotiated := tlsFront.ConnectionState().NegotiatedProtocol
	// HTTP/1.1 clients are permitted to omit ALPN. gRPC must negotiate h2.
	if negotiated != alpn && !(name == "http" && negotiated == "") {
		log.Printf("connection=%d rejected ALPN=%q expected=%q", id, negotiated, alpn)
		return
	}
	if err := front.SetDeadline(time.Time{}); err != nil {
		log.Printf("connection=%d clear deadline: %v", id, err)
		return
	}
	back, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", backend)
	if err != nil {
		log.Printf("connection=%d dial backend=%s: %v", id, backend, err)
		return
	}
	if !active.add(back) {
		return
	}
	defer active.remove(back)
	defer back.Close()
	log.Printf("connection=%d protocol=%s client=%s alpn=%s backend_local=%s backend_remote=%s",
		id, name, front.RemoteAddr(), negotiated, back.LocalAddr(), back.RemoteAddr())
	var copyWG sync.WaitGroup
	copyWG.Go(func() {
		if _, err := io.Copy(back, front); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("connection=%d client_to_backend: %v", id, err)
		}
		if tcpBack, ok := back.(*net.TCPConn); ok {
			_ = tcpBack.CloseWrite()
		}
	})
	if _, err := io.Copy(front, back); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("connection=%d backend_to_client: %v", id, err)
	}
	_ = front.Close()
	copyWG.Wait()
}
