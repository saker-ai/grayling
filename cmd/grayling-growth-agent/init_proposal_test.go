package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

func testInitProposalBinary(t *testing.T, binary string, goal growth.GrowthGoal) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SECRET-goal.json")
	goal.PrimaryMetric.Ref.Version = 1<<53 + 1
	goal.PrimaryMetric.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/" + goal.PrimaryMetric.Ref.Key + "/versions/9007199254740993"
	extra := goal.Guardrails[0]
	extra.Ref.Key = "latency"
	extra.SourcePath = "/api/v1/admin/workspaces/ws1/metrics/latency/versions/1"
	goal.Guardrails = append(append([]growth.ProposalMetric{}, goal.Guardrails...), extra)
	raw, err := json.Marshal(goal)
	if err != nil {
		t.Fatal(err)
	}
	write := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(raw)
	args := []string{"--goal", path, "--proposal-id", "new-proposal", "--proposal-version", "9007199254740993", "--workspace", "ws1", "--environment", "env1"}
	expected, err := growth.InitProposal(goal, "new-proposal", 1<<53+1, "ws1", "env1")
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, _ := json.Marshal(expected)
	expectedJSON = append(expectedJSON, '\n')
	invoke := func(args []string, want int, diagnostic string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"init-proposal"}, args...)...)
		cmd.Dir = dir
		cmd.Env = []string{"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "GRAYLING_GROWTH_TOKEN=SECRET"}
		var out, diag bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diag
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		if code != want || diag.String() != diagnostic || (want != 0 && out.Len() != 0) || strings.Contains(out.String()+diag.String(), "SECRET") {
			t.Fatalf("exit %d stdout %q stderr %q", code, out.String(), diag.String())
		}
		return out.Bytes()
	}
	got := invoke(args, 0, "")
	if !bytes.Equal(got, expectedJSON) || !bytes.Equal(got, invoke(args, 0, "")) {
		t.Fatal("unexpected construction output")
	}
	decoded, err := growth.DecodeProposal(bytes.NewReader(got))
	roundTrip, marshalErr := json.Marshal(decoded)
	if err != nil || marshalErr != nil || !bytes.Equal(append(roundTrip, '\n'), expectedJSON) {
		t.Fatal("decode", err)
	}
	verification, err := growth.VerifyGoal(goal, *decoded, "ws1", "env1")
	fp, fpErr := growth.ProposalFingerprint(*decoded)
	if err != nil || fpErr != nil || verification.ProposalFingerprint != fp || verification.PublicationAuthorized {
		t.Fatal("verification round trip", err)
	}
	if growth.ReviewProposal(*decoded, nil).State != growth.ProposalDraft {
		t.Fatal("not draft")
	}
	if p, err := growth.CompileCreateRequest(*decoded, nil, growth.PreparationInputs{}); p != nil || err == nil {
		t.Fatal("preparation allowed")
	}
	checkArtifactOutput(t, expectedJSON, 0, func(out, diag io.Writer) int {
		return run(t.Context(), append([]string{"init-proposal"}, args...), out, diag)
	})
	for i := 0; i < len(args); i += 2 {
		for _, duplicate := range [][]string{{args[i], args[i+1]}, {"-" + strings.TrimPrefix(args[i], "--") + "=SECRET"}} {
			invoke(append(append([]string{}, args...), duplicate...), 2, "invalid flags; use --help\n")
		}
		missing := append(append([]string{}, args[:i]...), args[i+2:]...)
		diagnostic := "invalid proposal initialization input\n"
		if i == 0 || i == 4 {
			diagnostic = "invalid initialization configuration\n"
		}
		invoke(missing, 2, diagnostic)
	}
	invoke(append(append([]string{}, args...), "SECRET"), 2, "invalid initialization configuration\n")
	equalsArgs := make([]string, 0, len(args)/2)
	for i := 0; i < len(args); i += 2 {
		equalsArgs = append(equalsArgs, args[i]+"="+args[i+1])
	}
	if !bytes.Equal(got, invoke(equalsArgs, 0, "")) {
		t.Fatal("equals spelling changed output")
	}
	for _, option := range []string{"--base-url=SECRET", "--token-env=SECRET", "--timeout=1s", "--token=SECRET"} {
		invoke(append(append([]string{}, args...), option), 2, "invalid flags; use --help\n")
	}
	for _, version := range []string{"0", "-1", "1.5", "1e3", "9223372036854775808", "SECRET"} {
		bad := append([]string{}, args...)
		bad[5] = version
		invoke(bad, 2, "invalid initialization configuration\n")
	}
	for _, index := range []int{7, 9} {
		bad := append([]string{}, args...)
		bad[index] = "other"
		invoke(bad, 2, "invalid proposal initialization input\n")
	}
	maxArgs := append([]string{}, args...)
	maxArgs[5] = "9223372036854775807"
	maxOutput := invoke(maxArgs, 0, "")
	maxProposal, err := growth.DecodeProposal(bytes.NewReader(maxOutput))
	if err != nil || maxProposal.Version != 9223372036854775807 {
		t.Fatal("lost maximum int64 version", err)
	}
	for _, index := range []int{3, 7, 9} {
		bad := append([]string{}, args...)
		bad[index] = "../SECRET"
		invoke(bad, 2, "invalid proposal initialization input\n")
	}
	for _, content := range []string{"", "null", "[]", `{"objective":"SECRET`, string(raw) + `{}`, `{"unknown":"SECRET",` + string(raw[1:]), `{"version":1,` + string(raw[1:]), strings.Replace(string(raw), `"primaryMetric":{`, `"primaryMetric":{"unknown":"SECRET",`, 1), strings.Replace(string(raw), `"version":9007199254740993`, `"version":9007199254740993.5`, 1), strings.Replace(string(raw), `"owner":"owner"`, `"owner":""`, 1), string(raw) + strings.Repeat(" ", growth.MaxGoalBytes), "\xff"} {
		write([]byte(content))
		invoke(args, 2, "invalid goal file or JSON\n")
	}
	write(raw)
	for _, kind := range []string{"symlink", "dangling", "loop", "directory", "fifo", "missing"} {
		rejected := filepath.Join(dir, kind)
		switch kind {
		case "symlink":
			err = os.Symlink(path, rejected)
		case "dangling":
			err = os.Symlink(filepath.Join(dir, "absent"), rejected)
		case "loop":
			err = os.Symlink(rejected, rejected)
		case "directory":
			err = os.Mkdir(rejected, 0700)
		case "fifo":
			err = syscall.Mkfifo(rejected, 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
		bad := append([]string{}, args...)
		bad[1] = rejected
		invoke(bad, 2, "invalid goal file or JSON\n")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Only fixture inputs exist: no sentinel from goal prose or command outputs.
	if len(entries) != 6 {
		t.Fatalf("unexpected file writes: %v", entries)
	}
}
