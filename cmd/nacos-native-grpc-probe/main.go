// Command nacos-native-grpc-probe checks that an anonymous native Nacos
// configuration read and subscription are rejected by a specific TLS/gRPC entrance IP.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	grpcsrv "github.com/godeps/gonacos/pkg/protocol/grpc"
	"golang.org/x/net/http2"
)

func main() {
	address := flag.String("address", "", "resolved TLS entrance IP:port")
	serverName := flag.String("server-name", "", "TLS server name and HTTP authority")
	caFile := flag.String("ca-file", "", "trusted CA PEM file")
	tenantA := flag.String("tenant-a-namespace", "", "first tenant namespace ID")
	tenantB := flag.String("tenant-b-namespace", "", "second tenant namespace ID")
	tokenA := flag.String("tenant-a-token-file", "", "first tenant SDK token file")
	tokenB := flag.String("tenant-b-token-file", "", "second tenant SDK token file")
	flag.Parse()
	if *address == "" || *serverName == "" || *caFile == "" || flag.NArg() != 0 ||
		(*tenantA == "") != (*tenantB == "") || (*tenantA == "") != (*tokenA == "") ||
		(*tenantB == "") != (*tokenB == "") {
		fmt.Fprintln(os.Stderr, "usage: nacos-native-grpc-probe -address IP:port -server-name DNS_NAME -ca-file CA.pem [-tenant-a-namespace ID -tenant-b-namespace ID -tenant-a-token-file FILE -tenant-b-token-file FILE]")
		os.Exit(2)
	}
	var tenants []tenant
	if *tenantA != "" {
		if *tenantA == *tenantB {
			fmt.Fprintln(os.Stderr, "ALERT: tenant namespace IDs must differ")
			os.Exit(2)
		}
		for _, input := range []struct{ namespace, path string }{{*tenantA, *tokenA}, {*tenantB, *tokenB}} {
			secret, err := readToken(input.path)
			if err != nil {
				fmt.Fprintln(os.Stderr, "ALERT: invalid SDK token file:", err)
				os.Exit(2)
			}
			tenants = append(tenants, tenant{namespace: input.namespace, token: secret})
		}
		if tenants[0].token == tenants[1].token {
			fmt.Fprintln(os.Stderr, "ALERT: tenant SDK tokens must differ")
			os.Exit(2)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := probe(ctx, *address, *serverName, *caFile, tenants); err != nil {
		fmt.Fprintln(os.Stderr, "ALERT:", err)
		os.Exit(1)
	}
	if len(tenants) == 0 {
		fmt.Println("PASS: anonymous native gRPC read and subscription rejected with status 16")
	} else {
		fmt.Println("PASS: anonymous gRPC denied; two tenant reads and subscriptions allowed; cross-tenant reads and subscriptions denied")
	}
}

type tenant struct {
	namespace string
	token     string
}

func readToken(path string) (string, error) {
	value, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimRight(string(value), "\r\n")
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("expected one nonempty SDK token line")
	}
	return token, nil
}

func probe(ctx context.Context, address, serverName, caFile string, tenants []tenant) error {
	ip, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid entrance address: %w", err)
	}
	if net.ParseIP(ip) == nil {
		return errors.New("entrance address must contain a resolved IP literal")
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return errors.New("CA file contains no trusted certificates")
	}
	newTransport := func() *http2.Transport {
		return &http2.Transport{
			DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
				var dialer net.Dialer
				conn, err := dialer.DialContext(ctx, "tcp", address)
				if err != nil {
					return nil, err
				}
				tlsConn := tls.Client(conn, &tls.Config{
					RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS12,
					NextProtos: []string{"h2"},
				})
				if err := tlsConn.HandshakeContext(ctx); err != nil {
					conn.Close()
					return nil, err
				}
				if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
					tlsConn.Close()
					return nil, errors.New("gRPC entrance did not negotiate h2")
				}
				return tlsConn, nil
			},
		}
	}
	transport := newTransport()
	defer transport.CloseIdleConnections()
	for _, check := range []struct {
		name, requestType string
		body              any
	}{
		{"query", "ConfigQueryRequest", map[string]string{
			"tenant": "golab-unauthorized-probe", "group": "GOLAB_FLAGS", "dataId": "golab.manifest",
		}},
		{"subscription", "ConfigBatchListenRequest", map[string]any{
			"listen": true,
			"configListenContexts": []map[string]string{{
				"tenant": "golab-unauthorized-probe", "group": "GOLAB_FLAGS",
				"dataId": "golab.manifest", "md5": "",
			}},
		}},
	} {
		if err := probeRequest(ctx, transport, serverName, port, check.name, check.requestType, check.body, "", grpcsrv.StatusUnauthenticated); err != nil {
			return err
		}
	}
	for _, current := range tenants {
		// A separate frontend TCP connection per tenant preserves backend identity
		// for subscriptions in the supported L4 topology.
		client := newTransport()
		defer client.CloseIdleConnections()
		for _, target := range tenants {
			expected := grpcsrv.StatusPermissionDenied
			if target.namespace == current.namespace {
				expected = grpcsrv.StatusOK
			}
			for _, check := range []struct {
				name, requestType string
				body              any
			}{
				{"query", "ConfigQueryRequest", map[string]string{
					"tenant": target.namespace, "group": "GOLAB_FLAGS", "dataId": "golab.manifest",
				}},
				{"subscription", "ConfigBatchListenRequest", map[string]any{
					"listen": true, "configListenContexts": []map[string]string{{
						"tenant": target.namespace, "group": "GOLAB_FLAGS", "dataId": "golab.manifest", "md5": "",
					}},
				}},
			} {
				if err := probeRequest(ctx, client, serverName, port, check.name, check.requestType, check.body, current.token, expected); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func probeRequest(ctx context.Context, transport *http2.Transport, serverName, port, name, requestType string, body any, token string, expected int) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	payload := grpcsrv.Payload{
		Metadata: grpcsrv.Metadata{Type: requestType, Headers: map[string]string{"accessToken": token}},
		Body: grpcsrv.Any{
			TypeURL: "type.googleapis.com/com.alibaba.nacos.api.config.request." + requestType,
			Value:   encoded,
		},
	}
	var frame bytes.Buffer
	if err := grpcsrv.WriteFrame(&frame, grpcsrv.Frame{Payload: payload.Encode()}); err != nil {
		return fmt.Errorf("encode %s gRPC frame: %w", name, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://"+net.JoinHostPort(serverName, port)+"/Request/request", &frame)
	if err != nil {
		return fmt.Errorf("create %s request: %w", name, err)
	}
	request.Header.Set("Content-Type", "application/grpc")
	response, err := transport.RoundTrip(request)
	if err != nil {
		return fmt.Errorf("gRPC %s: %w", name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("gRPC %s returned HTTP %d, expected 200", name, response.StatusCode)
	}
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "application/grpc") {
		return fmt.Errorf("%s did not receive a gRPC response", name)
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20)); err != nil {
		return fmt.Errorf("read %s gRPC response: %w", name, err)
	}
	status := response.Header.Get("grpc-status")
	if status == "" {
		status = response.Trailer.Get("grpc-status")
	}
	if status != strconv.Itoa(expected) {
		return fmt.Errorf("gRPC %s returned status %q, expected %d", name, status, expected)
	}
	return nil
}
