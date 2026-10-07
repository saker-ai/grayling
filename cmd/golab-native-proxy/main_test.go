package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoopbackBackend(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    bool
	}{
		{"127.0.0.1:18848", true},
		{"[::1]:19848", true},
		{"localhost:18848", false},
		{"0.0.0.0:18848", false},
		{"10.0.0.1:18848", false},
		{"127.0.0.1", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			if got := loopbackBackend(tc.address); got != tc.want {
				t.Fatalf("loopbackBackend(%q) = %v, want %v", tc.address, got, tc.want)
			}
		})
	}
}

func TestProxyRequiresALPNAndSeparateBackendConnections(t *testing.T) {
	certificate, der := testCertificate(t)
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	var accepted atomic.Int32
	var backendWG sync.WaitGroup
	backendWG.Go(func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	})
	defer func() {
		_ = backend.Close()
		backendWG.Wait()
	}()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	active := &activeConnections{conns: make(map[net.Conn]struct{})}
	slots := make(chan struct{}, 4)
	var connections, accepting sync.WaitGroup
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	accepting.Go(func() {
		serve(ctx, tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			NextProtos:   []string{"h2"}, MinVersion: tls.VersionTLS12,
		}), "grpc", backend.Addr().String(), "h2", active, slots, &connections)
	})
	defer func() {
		cancel()
		_ = listener.Close()
		accepting.Wait()
		active.closeAll()
		connections.Wait()
	}()

	for _, alpn := range []string{"", "http/1.1", "h2", "h2"} {
		config := &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "localhost", MinVersion: tls.VersionTLS12}
		config.RootCAs.AddCert(mustParseCertificate(t, der))
		if alpn != "" {
			config.NextProtos = []string{alpn}
		}
		conn, err := tls.Dial("tcp", listener.Addr().String(), config)
		if err != nil {
			// A mismatched ALPN may be rejected during the TLS handshake.
			if alpn == "h2" {
				t.Fatal(err)
			}
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		var one [1]byte
		_, readErr := conn.Read(one[:])
		_ = conn.Close()
		if readErr == nil {
			t.Fatalf("ALPN %q unexpectedly received backend data", alpn)
		}
		if alpn != "h2" && accepted.Load() != 0 {
			t.Fatalf("ALPN %q reached backend", alpn)
		}
	}
	deadline := time.Now().Add(time.Second)
	for accepted.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := accepted.Load(); got != 2 {
		t.Fatalf("backend accepted %d connections, want one per valid client", got)
	}
}

func TestHTTPWithoutALPNReachesBackend(t *testing.T) {
	certificate, der := testCertificate(t)
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	backendAccepted := make(chan struct{}, 1)
	var backendWG sync.WaitGroup
	backendWG.Go(func() {
		conn, err := backend.Accept()
		if err == nil {
			backendAccepted <- struct{}{}
			_ = conn.Close()
		}
	})
	defer func() {
		_ = backend.Close()
		backendWG.Wait()
	}()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	active := &activeConnections{conns: make(map[net.Conn]struct{})}
	var connections, accepting sync.WaitGroup
	ctx, cancel := context.WithCancel(t.Context())
	accepting.Go(func() {
		serve(ctx, tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate}, NextProtos: []string{"http/1.1"},
			MinVersion: tls.VersionTLS12,
		}), "http", backend.Addr().String(), "http/1.1", active, make(chan struct{}, 1), &connections)
	})
	defer func() {
		cancel()
		_ = listener.Close()
		accepting.Wait()
		active.closeAll()
		connections.Wait()
	}()
	roots := x509.NewCertPool()
	roots.AddCert(mustParseCertificate(t, der))
	conn, err := tls.Dial("tcp", listener.Addr().String(), &tls.Config{
		RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-backendAccepted:
	case <-time.After(time.Second):
		t.Fatal("HTTP client without ALPN did not reach backend")
	}
}

func testCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der
}

func mustParseCertificate(t *testing.T, der []byte) *x509.Certificate {
	t.Helper()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
