package main

import (
	"context"
	json "encoding/json/v2"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	grpcsrv "github.com/godeps/gonacos/pkg/protocol/grpc"
)

func TestProbeRequiresAnonymousReadAndSubscriptionDenial(t *testing.T) {
	for _, test := range []struct {
		name, queryStatus, listenStatus, contentType string
		wantOK                                       bool
	}{
		{name: "unauthenticated", queryStatus: "16", listenStatus: "16", contentType: "application/grpc", wantOK: true},
		{name: "configuration exposed", queryStatus: "0", listenStatus: "16", contentType: "application/grpc"},
		{name: "subscription exposed", queryStatus: "16", listenStatus: "0", contentType: "application/grpc"},
		{name: "missing grpc status"},
		{name: "false response", queryStatus: "16", listenStatus: "16", contentType: "text/plain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.ProtoMajor != 2 || r.URL.Path != "/Request/request" || r.Header.Get("Content-Type") != "application/grpc" {
					t.Errorf("unexpected request: %s %s %s", r.Proto, r.URL.Path, r.Header.Get("Content-Type"))
				}
				frame, err := grpcsrv.ReadFrame(r.Body)
				if err != nil {
					t.Errorf("read frame: %v", err)
				}
				payload, err := grpcsrv.DecodePayload(frame.Payload)
				if err != nil || payload.Metadata.Headers["accessToken"] != "" {
					t.Errorf("unexpected anonymous request: payload=%+v err=%v", payload.Metadata, err)
				}
				status := test.queryStatus
				switch payload.Metadata.Type {
				case "ConfigQueryRequest":
				case "ConfigBatchListenRequest":
					status = test.listenStatus
				default:
					t.Errorf("unexpected request type: %q", payload.Metadata.Type)
				}
				w.Header().Set("Content-Type", test.contentType)
				if status != "" {
					w.Header().Set("grpc-status", status)
				}
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			t.Cleanup(server.Close)
			certificate := server.Certificate()
			if certificate == nil {
				t.Fatal("test server has no certificate")
			}
			caFile := filepath.Join(t.TempDir(), "ca.pem")
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
			if err := os.WriteFile(caFile, ca, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := probe(ctx, server.Listener.Addr().String(), "127.0.0.1", caFile, nil)
			if (err == nil) != test.wantOK {
				t.Fatalf("probe error = %v, want success %v", err, test.wantOK)
			}
		})
	}
}

func TestProbeRequiresPinnedIPAddress(t *testing.T) {
	err := probe(t.Context(), "nacos.example:9848", "nacos.example", "/unused/ca.pem", nil)
	if err == nil || !strings.Contains(err.Error(), "resolved IP literal") {
		t.Fatalf("probe accepted DNS endpoint: %v", err)
	}
}

func TestProbeChecksTwoTenantIsolation(t *testing.T) {
	for _, test := range []struct {
		name      string
		crossCode int
		ownCode   int
		wantOK    bool
	}{
		{name: "isolated", crossCode: grpcsrv.StatusPermissionDenied, ownCode: grpcsrv.StatusOK, wantOK: true},
		{name: "cross tenant exposed", crossCode: grpcsrv.StatusOK, ownCode: grpcsrv.StatusOK},
		{name: "own tenant denied", crossCode: grpcsrv.StatusPermissionDenied, ownCode: grpcsrv.StatusPermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				frame, err := grpcsrv.ReadFrame(r.Body)
				if err != nil {
					t.Errorf("read frame: %v", err)
					return
				}
				payload, err := grpcsrv.DecodePayload(frame.Payload)
				if err != nil {
					t.Errorf("decode frame: %v", err)
					return
				}
				var body struct {
					Tenant   string `json:"tenant"`
					Contexts []struct {
						Tenant string `json:"tenant"`
					} `json:"configListenContexts"`
				}
				if err := json.Unmarshal(payload.Body.Value, &body); err != nil {
					t.Errorf("decode body: %v", err)
					return
				}
				namespace := body.Tenant
				if payload.Metadata.Type == "ConfigBatchListenRequest" {
					if len(body.Contexts) != 1 {
						t.Errorf("unexpected contexts: %+v", body.Contexts)
						return
					}
					namespace = body.Contexts[0].Tenant
				}
				status := grpcsrv.StatusUnauthenticated
				if token := payload.Metadata.Headers["accessToken"]; token != "" {
					status = test.crossCode
					if (token == "token-a" && namespace == "namespace-a") || (token == "token-b" && namespace == "namespace-b") {
						status = test.ownCode
					}
				}
				w.Header().Set("Content-Type", "application/grpc")
				w.Header().Set("grpc-status", strconv.Itoa(status))
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			t.Cleanup(server.Close)
			caFile := filepath.Join(t.TempDir(), "ca.pem")
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			if err := os.WriteFile(caFile, ca, 0o600); err != nil {
				t.Fatal(err)
			}
			err := probe(t.Context(), server.Listener.Addr().String(), "127.0.0.1", caFile, []tenant{
				{namespace: "namespace-a", token: "token-a"},
				{namespace: "namespace-b", token: "token-b"},
			})
			if (err == nil) != test.wantOK {
				t.Fatalf("probe error = %v, want success %v", err, test.wantOK)
			}
		})
	}
}
