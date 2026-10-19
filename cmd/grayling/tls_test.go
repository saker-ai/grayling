package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestListenerTLS(t *testing.T) {
	if config, err := listenerTLS(false, "", ""); err != nil || config != nil {
		t.Fatalf("default listener: %v", err)
	}
	for _, files := range [][2]string{{"", ""}, {"certificate", ""}, {"", "key"}, {"missing", "missing"}} {
		if _, err := listenerTLS(true, files[0], files[1]); err == nil {
			t.Fatalf("accepted incomplete Agent TLS configuration: %v", files)
		}
	}
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	certificate := server.TLS.Certificates[0]
	private, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "certificate.pem")
	keyPath := filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := listenerTLS(true, certificatePath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion != tls.VersionTLS13 || len(config.Certificates) != 1 {
		t.Fatal("listener TLS configuration lost security bounds")
	}
}
