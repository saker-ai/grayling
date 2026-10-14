// Package traefiktest resolves a REAL Traefik ≥3.6 official binary for the
// gateway end-to-end tests (docs/技术方案.md §12.4: 用 Traefik ≥3.6 固定版本
// 验证 multi-layer routing 链路). No mocks, no substitutes: the tests run the
// official linux/amd64 release build from GitHub with a verified SHA-256.
//
// Resolution order:
//
//  1. GRAYLING_TEST_TRAEFIK_BIN — an explicit traefik binary path (must be ≥3.6).
//  2. Cache — $GRAYLING_TEST_TRAEFIK_DIR or os.UserCacheDir()/grayling-test-bin,
//     downloaded once from the official GitHub release and checksum-verified.
//  3. Neither available (no network, non-linux/amd64) — tests skip with an
//     explicit reason.
package traefiktest

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Pinned official release (recorded in docs/实施状态.md §4 evidence).
const (
	Version        = "v3.7.13"
	releaseAsset   = "traefik_v3.7.13_linux_amd64.tar.gz"
	releaseURL     = "https://github.com/traefik/traefik/releases/download/v3.7.13/" + releaseAsset
	releaseSHA256  = "52cd039a34258dd61c617a95d69252bc6bcae27c520f338186c31c7fef8f6394"
	minMajor       = 3
	minMinor       = 6
	binaryFileName = "traefik"
)

// Binary returns the path to a verified Traefik ≥3.6 binary, downloading the
// pinned official release into the cache on first use. It skips the test with
// an explicit reason when no binary can be provided.
func Binary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("GRAYLING_TEST_TRAEFIK_BIN"); bin != "" {
		checkVersion(t, bin)
		return bin
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skipf("traefiktest: pinned release %s is linux/amd64 (host: %s/%s) — set GRAYLING_TEST_TRAEFIK_BIN",
			Version, runtime.GOOS, runtime.GOARCH)
	}
	dir := os.Getenv("GRAYLING_TEST_TRAEFIK_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			home, herr := os.UserHomeDir()
			if herr != nil {
				t.Skipf("traefiktest: no cache dir: %v", herr)
			}
			cache = filepath.Join(home, ".cache")
		}
		dir = filepath.Join(cache, "grayling-test-bin")
	}
	bin := filepath.Join(dir, "traefik-"+strings.TrimPrefix(Version, "v"))
	if _, err := os.Stat(bin); err == nil {
		checkVersion(t, bin)
		return bin
	}
	download(t, bin)
	checkVersion(t, bin)
	return bin
}

// VersionString returns the full `traefik version` output for evidence logs.
func VersionString(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("traefik version: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// checkVersion asserts the binary self-reports ≥3.6 (multi-layer routing).
func checkVersion(t *testing.T, bin string) {
	t.Helper()
	out := VersionString(t, bin)
	major, minor := parseVersion(t, out)
	if major < minMajor || (major == minMajor && minor < minMinor) {
		t.Fatalf("traefik %d.%d < required %d.%d (multi-layer routing):\n%s",
			major, minor, minMajor, minMinor, out)
	}
}

func parseVersion(t *testing.T, versionOutput string) (major, minor int) {
	t.Helper()
	for _, line := range strings.Split(versionOutput, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Version:") {
			continue
		}
		v := strings.TrimSpace(strings.TrimPrefix(line, "Version:"))
		v = strings.TrimPrefix(v, "v")
		parts := strings.Split(v, ".")
		if len(parts) < 2 {
			continue
		}
		mj, err1 := strconv.Atoi(parts[0])
		mn, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil {
			return mj, mn
		}
	}
	t.Fatalf("cannot parse traefik version from:\n%s", versionOutput)
	return 0, 0
}

// download fetches the pinned release tarball, verifies its SHA-256 against
// the published checksum and extracts the binary atomically (tmp → rename).
func download(t *testing.T, dest string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Skipf("traefiktest: cache dir: %v", err)
	}
	t.Logf("traefiktest: downloading official %s from %s", Version, releaseURL)
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(releaseURL)
	if err != nil {
		t.Skipf("traefiktest: cannot download %s (offline?): %v — set GRAYLING_TEST_TRAEFIK_BIN", releaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("traefiktest: download %s: HTTP %d — set GRAYLING_TEST_TRAEFIK_BIN", releaseURL, resp.StatusCode)
	}
	tmp := dest + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		t.Fatalf("traefiktest: create tmp: %v", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		t.Skipf("traefiktest: download interrupted: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("traefiktest: close tmp: %v", err)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != releaseSHA256 {
		_ = os.Remove(tmp)
		t.Fatalf("traefiktest: SHA-256 mismatch for %s:\n got %s\nwant %s", releaseAsset, got, releaseSHA256)
	}
	t.Logf("traefiktest: verified sha256=%s (%d bytes)", got, hasher.Size())
	if err := extractTraefik(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		t.Fatalf("traefiktest: extract: %v", err)
	}
	_ = os.Remove(tmp)
}

// extractTraefik pulls the "traefik" member out of the release tar.gz and
// installs it at dest (0755).
func extractTraefik(tarball, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("traefik binary not found in %s", tarball)
		}
		if err != nil {
			return err
		}
		if filepath.Base(hdr.Name) != binaryFileName || hdr.Typeflag != tar.TypeReg {
			continue
		}
		tmp := dest + ".extract"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			_ = os.Remove(tmp)
			return err
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		return os.Rename(tmp, dest)
	}
}
