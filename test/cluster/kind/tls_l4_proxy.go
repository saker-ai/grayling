// A test-only TLS terminator that preserves one backend TCP connection per client.
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

func main() {
	httpListen := flag.String("http-listen", ":8848", "TLS HTTP listener")
	grpcListen := flag.String("grpc-listen", ":9848", "TLS gRPC listener")
	httpBackend := flag.String("http-backend", "127.0.0.1:18848", "HTTP backend")
	grpcBackend := flag.String("grpc-backend", "127.0.0.1:19848", "gRPC backend")
	certFile := flag.String("cert", "/tls/tls.crt", "TLS certificate")
	keyFile := flag.String("key", "/tls/tls.key", "TLS private key")
	flag.Parse()

	certificate, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listeners := make([]net.Listener, 0, 2)
	for _, endpoint := range []struct {
		name, address, backend, alpn string
	}{
		{"http", *httpListen, *httpBackend, "http/1.1"},
		{"grpc", *grpcListen, *grpcBackend, "h2"},
	} {
		listener, listenErr := net.Listen("tcp", endpoint.address)
		if listenErr != nil {
			log.Fatal(listenErr)
		}
		listeners = append(listeners, listener)
		tlsListener := tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
			NextProtos:   []string{endpoint.alpn},
		})
		go serve(tlsListener, endpoint.name, endpoint.backend)
	}
	log.Printf("ready http=%s grpc=%s", *httpListen, *grpcListen)
	<-ctx.Done()
	for _, listener := range listeners {
		if err := listener.Close(); err != nil {
			log.Printf("close listener: %v", err)
		}
	}
}

func serve(listener net.Listener, name, backend string) {
	for {
		front, err := listener.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("accept %s: %v", name, err)
			}
			return
		}
		go proxy(front, name, backend)
	}
}

func proxy(front net.Conn, name, backend string) {
	defer front.Close()
	id := nextConnection.Add(1)
	if err := front.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		log.Printf("connection=%d deadline: %v", id, err)
		return
	}
	tlsFront := front.(*tls.Conn)
	if err := tlsFront.Handshake(); err != nil {
		log.Printf("connection=%d handshake: %v", id, err)
		return
	}
	if err := front.SetDeadline(time.Time{}); err != nil {
		log.Printf("connection=%d clear deadline: %v", id, err)
		return
	}
	back, err := net.DialTimeout("tcp", backend, 5*time.Second)
	if err != nil {
		log.Printf("connection=%d dial backend=%s: %v", id, backend, err)
		return
	}
	defer back.Close()
	log.Printf("connection=%d protocol=%s client=%s alpn=%s backend_local=%s backend_remote=%s",
		id, name, front.RemoteAddr(), tlsFront.ConnectionState().NegotiatedProtocol,
		back.LocalAddr(), back.RemoteAddr())

	var copyWG sync.WaitGroup
	copyWG.Go(func() {
		if _, err := io.Copy(back, front); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("connection=%d client_to_backend: %v", id, err)
		}
		if err := back.(*net.TCPConn).CloseWrite(); err != nil {
			log.Printf("connection=%d backend close write: %v", id, err)
		}
	})
	if _, err := io.Copy(front, back); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("connection=%d backend_to_client: %v", id, err)
	}
	if err := front.Close(); err != nil {
		log.Printf("connection=%d client close: %v", id, err)
	}
	copyWG.Wait()
}
