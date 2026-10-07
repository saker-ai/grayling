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

// Exercise two credentials over exactly one backend HTTP/2 TCP connection.
// This is a negative probe for L7 pooling, not support for an L7 proxy: a
// second tenant cannot attach a subscription to the first tenant's transport.
func checkNativeSharedBackendConnection(t *testing.T, backend, namespaceA, tokenA, tokenB string, cert tls.Certificate, caFile string) {
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
	listen := func(token string) int {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"listen": true,
			"configListenContexts": []map[string]string{{
				"tenant": namespaceA, "group": configstore.Group,
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
	if status := listen(tokenA); status != grpcsrv.StatusOK {
		t.Fatalf("first tenant subscription on shared transport: gRPC status %d", status)
	}
	if status := listen(tokenB); status != grpcsrv.StatusPermissionDenied {
		t.Fatalf("second tenant subscription on shared transport: gRPC status %d, want 7", status)
	}
	if count := accepted.Load(); count != 1 {
		t.Fatalf("expected one shared backend TCP connection, got %d", count)
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
