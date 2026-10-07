package cluster

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	json "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/godeps/golab/internal/configstore"
	grpcsrv "github.com/godeps/gonacos/pkg/protocol/grpc"
	"golang.org/x/net/http2"
)

// Exercise two credentials over one shared backend HTTP/2 TCP connection,
// then give the second tenant a separate connection.
// This is a negative probe for L7 pooling, not support for an L7 proxy: a
// cross-tenant reads are denied on a shared transport. A second tenant can
// read its own namespace but cannot attach its own subscription to the first
// tenant's connection. A separate connection must permit that subscription.
func checkNativeSharedBackendConnection(t *testing.T, backend, namespaceA, namespaceB, tokenA, tokenB string, cert tls.Certificate, caFile string) {
	t.Helper()
	var accepted atomic.Int64
	addr := startNativeTLSL4Count(t, backend, cert, true, &accepted)
	ca, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		t.Fatal("invalid test CA")
	}
	transport := &http2.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	if status := nativeRawTLSQueryStatus(t, client, addr, namespaceA, tokenA); status != grpcsrv.StatusOK {
		t.Fatalf("first tenant read on shared transport: gRPC status %d", status)
	}
	if status := nativeRawTLSQueryStatus(t, client, addr, namespaceA, tokenB); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("second tenant read on shared transport: gRPC status %d, want 7", status)
	}
	if status := nativeRawTLSQueryStatus(t, client, addr, namespaceB, tokenB); status != grpcsrv.StatusOK {
		t.Fatalf("second tenant own-namespace read on shared transport: gRPC status %d, want 0", status)
	}
	if status := nativeRawTLSQueryStatus(t, client, addr, namespaceA, tokenA); status != grpcsrv.StatusOK {
		t.Fatalf("first tenant read after rejected requests: gRPC status %d, want 0", status)
	}
	listen := func(client *http.Client, namespace, token string) int {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"listen": true,
			"configListenContexts": []map[string]string{{
				"tenant": namespace, "group": configstore.Group,
				"dataId": configstore.ManifestDataID, "md5": "",
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		payload := grpcsrv.Payload{
			Metadata: grpcsrv.Metadata{Type: "ConfigBatchListenRequest", Headers: map[string]string{"accessToken": token}},
			Body:     grpcsrv.Any{TypeURL: "type.googleapis.com/com.alibaba.nacos.api.config.request.ConfigBatchListenRequest", Value: body},
		}
		var frame bytes.Buffer
		if err := grpcsrv.WriteFrame(&frame, grpcsrv.Frame{Payload: payload.Encode()}); err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+addr+"/Request/request", &frame)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/grpc")
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		status, err := strconv.Atoi(response.Header.Get("grpc-status"))
		if err != nil {
			t.Fatal(fmt.Errorf("missing gRPC status: %w", err))
		}
		return status
	}
	if status := listen(client, namespaceA, ""); status != grpcsrv.StatusUnauthenticated {
		t.Fatalf("anonymous direct TLS gRPC subscription: status %d, want 16", status)
	}
	if status := listen(client, namespaceA, tokenA); status != grpcsrv.StatusOK {
		t.Fatalf("first tenant subscription on shared transport: gRPC status %d", status)
	}
	if status := listen(client, namespaceA, tokenB); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("second tenant subscription on shared transport: gRPC status %d, want 7", status)
	}
	if status := listen(client, namespaceB, tokenB); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("second tenant own-namespace subscription on shared transport: gRPC status %d, want 7", status)
	}
	if count := accepted.Load(); count != 1 {
		t.Fatalf("expected one shared backend TCP connection, got %d", count)
	}
	separateTransport := &http2.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	t.Cleanup(separateTransport.CloseIdleConnections)
	separateClient := &http.Client{Transport: separateTransport}
	if status := listen(separateClient, namespaceB, tokenB); status != grpcsrv.StatusOK {
		t.Fatalf("second tenant own-namespace subscription on separate connection: gRPC status %d, want 0", status)
	}
	if count := accepted.Load(); count != 2 {
		t.Fatalf("separate tenant subscription used %d backend TCP connections, want two", count)
	}
}

// Query the server directly so an SDK last-known-good cache cannot turn a
// rejected read into an apparent success. Reusing this client also reuses its
// HTTP/2 connection across the credential revocation.
func nativeRawTLSQueryStatus(t *testing.T, client *http.Client, addr, namespace, token string) int {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"tenant": namespace, "group": configstore.Group, "dataId": configstore.ManifestDataID,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := grpcsrv.Payload{
		Metadata: grpcsrv.Metadata{Type: "ConfigQueryRequest", Headers: map[string]string{"accessToken": token}},
		Body:     grpcsrv.Any{TypeURL: "type.googleapis.com/com.alibaba.nacos.api.config.request.ConfigQueryRequest", Value: body},
	}
	var frame bytes.Buffer
	if err := grpcsrv.WriteFrame(&frame, grpcsrv.Frame{Payload: payload.Encode()}); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+addr+"/Request/request", &frame)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/grpc")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	status, err := strconv.Atoi(response.Header.Get("grpc-status"))
	if err != nil {
		t.Fatalf("missing gRPC status: %v", err)
	}
	return status
}
