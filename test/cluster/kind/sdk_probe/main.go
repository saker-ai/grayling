// Command sdk_probe runs the official Nacos Go client against the Kind fixture.
package main

import (
	"crypto/tls"
	"crypto/x509"
	json "encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/godeps/golab/pkg/model"
	"github.com/nacos-group/nacos-sdk-go/v2/clients"
	"github.com/nacos-group/nacos-sdk-go/v2/common/constant"
	"github.com/nacos-group/nacos-sdk-go/v2/vo"
)

func main() {
	server := flag.String("server", "golab-a", "TLS service DNS name")
	namespaceFile := flag.String("namespace-file", "", "namespace ID file")
	tokenFile := flag.String("token-file", "", "SDK token file")
	caFile := flag.String("ca-file", "", "trusted CA certificate")
	denied := flag.Bool("expect-denied", false, "expect the native read to be denied")
	watch := flag.Bool("watch", false, "wait for a newer manifest notification")
	flag.Parse()
	if *namespaceFile == "" || *tokenFile == "" || *caFile == "" || (*denied && *watch) {
		fmt.Fprintln(os.Stderr, "namespace-file, token-file, and ca-file are required; denied and watch are exclusive")
		os.Exit(2)
	}
	namespace := readFile(*namespaceFile)
	token := readFile(*tokenFile)
	if err := verifyHTTP(*server, namespace, *caFile); err != nil {
		fail("native HTTP boundary", err)
	}
	fmt.Printf("PASS native HTTP bypass and invalid login denied through %s\n", *server)
	cache, err := os.MkdirTemp("", "golab-kind-sdk-")
	if err != nil {
		fail("create SDK cache", err)
	}
	defer os.RemoveAll(cache)

	client, err := clients.NewConfigClient(vo.NacosClientParam{
		ClientConfig: constant.NewClientConfig(
			constant.WithNamespaceId(namespace), constant.WithTimeoutMs(3000),
			constant.WithUsername("golab-sdk"), constant.WithPassword(token),
			constant.WithNotLoadCacheAtStart(true), constant.WithCacheDir(cache),
			constant.WithLogDir(cache),
			constant.WithTLS(*constant.NewTLSConfig(constant.WithCA(*caFile, ""))),
		),
		ServerConfigs: []constant.ServerConfig{
			*constant.NewServerConfig(*server, 8848, constant.WithScheme("https"), constant.WithGrpcPort(9848)),
		},
	})
	if err != nil {
		fail("create official SDK client", err)
	}
	defer client.CloseClient()

	param := vo.ConfigParam{DataId: "golab.manifest", Group: "GOLAB_FLAGS"}
	content, err := client.GetConfig(param)
	if *denied {
		if err == nil {
			fail("cross-tenant read succeeded", nil)
		}
		fmt.Println("PASS official SDK cross-tenant read denied")
		return
	}
	if err != nil {
		fail("read manifest", err)
	}
	initial := parseRevision(content)
	if initial <= 0 {
		fail("manifest has no published revision", nil)
	}
	fmt.Printf("PASS official SDK read revision %d through %s\n", initial, *server)
	if !*watch {
		return
	}

	updates := make(chan int64, 8)
	param.OnChange = func(_, _, _, body string) {
		var manifest model.Manifest
		if json.Unmarshal([]byte(body), &manifest) == nil {
			select {
			case updates <- manifest.Revision:
			default:
			}
		}
	}
	if err := client.ListenConfig(param); err != nil {
		fail("subscribe manifest", err)
	}
	fmt.Println("SUBSCRIBED")
	timer := time.NewTimer(35 * time.Second)
	defer timer.Stop()
	for {
		select {
		case revision := <-updates:
			if revision > initial {
				fmt.Printf("PASS official SDK received revision %d through %s\n", revision, *server)
				return
			}
		case <-timer.C:
			fail("timed out waiting for manifest push", nil)
		}
	}
}

func verifyHTTP(server, namespace, caFile string) error {
	certificate, err := os.ReadFile(caFile)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		return fmt.Errorf("invalid CA certificate")
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs: roots, MinVersion: tls.VersionTLS12,
		}},
	}
	query := url.Values{"tenant": {namespace}, "group": {"GOLAB_FLAGS"}, "dataId": {"golab.manifest"}}
	endpoint := "https://" + server + ":8848"
	for _, check := range []struct {
		request *http.Request
		want    int
	}{
		{request: mustRequest(http.MethodGet, endpoint+"/nacos/v1/cs/configs?"+query.Encode()), want: http.StatusNotFound},
		{request: mustRequest(http.MethodPost, endpoint+"/nacos/v1/auth/users/login"), want: http.StatusUnauthorized},
	} {
		response, requestErr := client.Do(check.request)
		if requestErr != nil {
			return requestErr
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
		if closeErr := response.Body.Close(); closeErr != nil {
			return closeErr
		}
		if response.StatusCode != check.want {
			return fmt.Errorf("%s %s returned %d, want %d", check.request.Method,
				check.request.URL.Path, response.StatusCode, check.want)
		}
	}
	return nil
}

func mustRequest(method, endpoint string) *http.Request {
	request, err := http.NewRequest(method, endpoint, nil)
	if err != nil {
		fail("construct HTTP probe", err)
	}
	return request
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		fail("read credential file", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		fail("empty credential file", nil)
	}
	return value
}

func parseRevision(content string) int64 {
	var manifest model.Manifest
	if err := json.Unmarshal([]byte(content), &manifest); err != nil {
		fail("decode manifest", err)
	}
	return manifest.Revision
}

func fail(message string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL %s: %v\n", message, err)
	} else {
		fmt.Fprintf(os.Stderr, "FAIL %s\n", message)
	}
	os.Exit(1)
}
