package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

// Reuse the verify-goal binary and goal fixture, including versions above 2^53.
func testFingerprintGoal(t *testing.T, binary string, goal growth.GrowthGoal) {
	dir := t.TempDir()
	path := filepath.Join(dir, "goal.json")
	data, err := json.Marshal(goal)
	if err != nil {
		t.Fatal(err)
	}
	write := func(raw []byte) {
		t.Helper()
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(data)
	run := func(args []string, want int, expected string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, append([]string{"fingerprint-goal"}, args...)...)
		cmd.Dir = dir
		// No token, scope or proposal; unusable proxies ensure no network dependency.
		cmd.Env = []string{"HTTP_PROXY=http://127.0.0.1:1", "HTTPS_PROXY=http://127.0.0.1:1", "ALL_PROXY=http://127.0.0.1:1"}
		var out, diag bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diag
		err := cmd.Run()
		code := 0
		if err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		if code != want || (want == 0 && (out.String() != expected+"\n" || diag.Len() != 0)) ||
			(want != 0 && (out.Len() != 0 || diag.String() != expected+"\n" || diag.Len() > 128)) {
			t.Fatalf("exit %d want %d stdout %q stderr %q", code, want, out.String(), diag.String())
		}
	}
	args := []string{"--goal", path}
	fp, err := growth.GoalFingerprint(goal)
	if err != nil {
		t.Fatal(err)
	}
	run(args, 0, fp)
	run(args, 0, fp)
	run([]string{"--goal=" + path}, 0, fp)
	// JSON member order and whitespace do not affect the hash.
	reordered := append([]byte(`{"version":9007199254740993,`), bytes.Replace(data[1:], []byte(`"version":9007199254740993,`), nil, 1)...)
	write(append([]byte(" \n"), reordered...))
	run(args, 0, fp)
	for name, change := range map[string]func(*growth.GrowthGoal){
		"adjacent exact version": func(g *growth.GrowthGoal) { g.Version-- },
		"same version content":   func(g *growth.GrowthGoal) { g.Objective += " changed" },
		"exact metric version": func(g *growth.GrowthGoal) {
			g.PrimaryMetric.Ref.Version = 1<<53 + 1
			g.PrimaryMetric.SourcePath = "/api/v1/admin/workspaces/" + g.WorkspaceID + "/metrics/" + g.PrimaryMetric.Ref.Key + "/versions/9007199254740993"
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := goal
			change(&changed)
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := growth.GoalFingerprint(changed)
			if err != nil || expected == fp {
				t.Fatal("content change did not change hash", err)
			}
			write(raw)
			run(args, 0, expected)
		})
	}
	s := string(data)
	for name, raw := range map[string]string{
		"empty": "", "malformed": `{"objective":"SECRET`, "null": "null", "array": "[]",
		"unknown":           `{"execute":"SECRET",` + s[1:],
		"duplicate":         `{"version":1,` + s[1:],
		"escaped duplicate": `{"\u0076ersion":1,` + s[1:],
		"nested unknown":    strings.Replace(s, `"primaryMetric":{`, `"primaryMetric":{"execute":true,`, 1),
		"nested duplicate":  strings.Replace(s, `"primaryMetric":{`, `"primaryMetric":{"sourcePath":"SECRET",`, 1),
		"trailing":          s + `{}`, "utf8": strings.Replace(s, "objective", "\xff", 1),
		"oversize":           s + strings.Repeat(" ", growth.MaxGoalBytes),
		"invalid version":    strings.Replace(s, `"version":9007199254740993`, `"version":0`, 1),
		"fractional version": strings.Replace(s, `"version":9007199254740993`, `"version":1.5`, 1),
		"overflow version":   strings.Replace(s, `"version":9007199254740993`, `"version":9223372036854775808`, 1),
	} {
		t.Run(name, func(t *testing.T) { write([]byte(raw)); run(args, 2, "invalid goal file or JSON") })
	}
	write(append(append([]byte{}, data...), bytes.Repeat([]byte(" "), growth.MaxGoalBytes-len(data))...))
	run(args, 0, fp)
	write(data)
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{fifo, link, dir, filepath.Join(dir, "SECRET-absent"), "/dev/null"} {
		run([]string{"--goal", bad}, 2, "invalid goal file or JSON")
	}
	for _, bad := range [][]string{nil, {"--goal="}, {"--goal", path, "SECRET"}} {
		run(bad, 2, "invalid fingerprint configuration")
	}
	for _, duplicate := range [][]string{{"--goal", path, "--goal", path}, {"--goal=" + path, "--goal=" + path}} {
		run(duplicate, 2, "invalid flags; use --help")
	}
	run([]string{"--goal"}, 2, "invalid flags; use --help")
	for _, flag := range []string{"--base-url", "--token-env", "--timeout", "--workspace", "--environment", "--proposal"} {
		run(append(append([]string{}, args...), flag, "SECRET"), 2, "invalid flags; use --help")
	}
	for _, writer := range []interface{ Write([]byte) (int, error) }{reviewFailWriter{}, goalShortWriter{}} {
		var diag bytes.Buffer
		if code := runFingerprintGoal(args, writer, &diag); code != 1 || diag.String() != "output failed\n" {
			t.Fatalf("write: %d %s", code, &diag)
		}
	}
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	cmd := exec.CommandContext(t.Context(), binary, append([]string{"fingerprint-goal"}, args...)...)
	cmd.Env = []string{}
	cmd.Stdout = full
	var diag bytes.Buffer
	cmd.Stderr = &diag
	var ee *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &ee) || ee.ExitCode() != 1 || diag.String() != "output failed\n" {
		t.Fatalf("output failure: %v %s", err, &diag)
	}
	if _, err := os.Stat(filepath.Join(dir, "sentinel")); !os.IsNotExist(err) {
		t.Fatal("executed goal prose")
	}
}
