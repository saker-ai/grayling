package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/saker-ai/grayling/internal/growth"
)

func testFingerprint(t *testing.T, binary string) {
	p := fixtureProposal(t)
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := growth.ProposalFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid", "incomplete", "missing", "fifo", "directory", "oversized", "null", "array", "malformed", "duplicate", "escaped duplicate", "trailing", "authorization", "nested evidence", "network flag"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proposal.json")
			raw := string(data)
			want := 2
			expected := fp
			extra := []string{}
			switch name {
			case "valid":
				want = 0
			case "incomplete":
				incomplete := growth.Proposal{}
				b, err := json.Marshal(incomplete)
				if err != nil {
					t.Fatal(err)
				}
				raw = string(b)
				expected, err = growth.ProposalFingerprint(incomplete)
				if err != nil {
					t.Fatal(err)
				}
				want = 0
			case "missing":
				path = filepath.Join(t.TempDir(), "absent")
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				path = t.TempDir()
			case "oversized":
				raw = strings.Repeat(" ", growth.MaxProposalBytes+1)
			case "null":
				raw = "null"
			case "array":
				raw = "[]"
			case "malformed":
				raw = `{"id":"` + secret
			case "duplicate":
				raw = strings.Replace(raw, `{`, `{"id":"duplicate",`, 1)
			case "escaped duplicate":
				raw = strings.Replace(raw, `{`, `{"\u0069d":"duplicate",`, 1)
			case "trailing":
				raw += `{}`
			case "authorization":
				raw = strings.Replace(raw, `{`, `{"approved":true,`, 1)
			case "nested evidence":
				raw = strings.Replace(raw, `"primaryMetric":{`, `"primaryMetric":{"evidence":{"status":"READY"},`, 1)
			case "network flag":
				extra = []string{"--base-url", "http://127.0.0.1:1"}
			}
			if name != "missing" && name != "fifo" && name != "directory" {
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := append([]string{"fingerprint", "--proposal", path}, extra...)
			cmd := exec.CommandContext(t.Context(), binary, args...)
			cmd.Env = append(os.Environ(), "GRAYLING_GROWTH_TOKEN=")
			var out, diagnostics bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &diagnostics
			err := cmd.Run()
			code := 0
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = ee.ExitCode()
			}
			if code != want || (want == 0 && (out.String() != expected+"\n" || diagnostics.Len() != 0)) || (want != 0 && (out.Len() != 0 || diagnostics.Len() == 0 || diagnostics.Len() > 256)) || strings.Contains(diagnostics.String(), secret) {
				t.Fatalf("exit %d want %d stdout %q stderr %q", code, want, out.String(), diagnostics.String())
			}
		})
	}
}
