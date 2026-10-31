package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/saker-ai/grayling/internal/growth"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseFlagsSemantics(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          []string
		want          string
		rest          []string
		help, invalid bool
	}{
		{name: "flag-looking value", args: []string{"--text", "--text"}, want: "--text"},
		{name: "help value", args: []string{"-text", "--help"}, want: "--help"},
		{name: "terminator value", args: []string{"--text", "--"}, want: "--"},
		{name: "terminator", args: []string{"--", "--text=a", "--text=b"}, rest: []string{"--text=a", "--text=b"}},
		{name: "positional", args: []string{"operand", "--text=a", "--text=b"}, rest: []string{"operand", "--text=a", "--text=b"}},
		{name: "help first", args: []string{"--help", "--text=a", "--text=b"}, help: true},
		{name: "help after value", args: []string{"--text=a", "-h", "--text=b"}, want: "a", help: true},
		{name: "duplicate before help", args: []string{"--text=a", "-text=b", "--help"}, want: "a", invalid: true},
		{name: "duplicate boolean", args: []string{"--enabled", "-enabled=false"}, invalid: true},
		{name: "missing value", args: []string{"--text"}, invalid: true},
		{name: "unknown flag", args: []string{"--unknown"}, invalid: true},
		{name: "invalid duration", args: []string{"--timeout=secret"}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			value := fs.String("text", "", "")
			fs.Bool("enabled", false, "")
			fs.Duration("timeout", time.Second, "")
			original := fs.Lookup("text").Value
			err := parseFlags(fs, tc.args)
			if errors.Is(err, flag.ErrHelp) != tc.help || (err != nil && !errors.Is(err, flag.ErrHelp)) != tc.invalid || *value != tc.want {
				t.Fatalf("err=%v value=%q rest=%v", err, *value, fs.Args())
			}
			if err == nil && !slices.Equal(fs.Args(), tc.rest) {
				t.Fatalf("rest=%v want=%v", fs.Args(), tc.rest)
			}
			if fs.Lookup("text").Value != original {
				t.Fatal("value wrapper was not restored")
			}
		})
	}
}

// Exercise every registered singleton, including offline artifact selectors and
// scope/origin/credential flags, both in-process and through the real executable.
func TestSingletonFlagsCommands(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "grayling-growth-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, b)
	}
	var requests, nonGET atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			nonGET.Add(1)
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	online := "base-url workspace environment token-env timeout"
	commands := []struct{ name, flags string }{
		{"review", "proposal goal " + online},
		{"prepare", "proposal goal settings review-record " + online},
		{"record-review", "proposal goal review-input " + online},
		{"record-learning", "proposal goal manifest learning-input " + online},
		{"bind-check", "manifest " + online}, {"rollout", "manifest " + online}, {"monitor", "manifest " + online},
		{"verify-goal", "goal proposal workspace environment"},
		{"verify-review", "proposal goal record workspace environment"},
		{"verify-learning", "proposal goal record workspace environment"},
		{"search-learning", "workspace environment text"},
		{"fingerprint", "proposal"}, {"fingerprint-goal", "goal"},
	}
	execute := func(t *testing.T, args []string, binaryMode bool) (int, string, string) {
		t.Helper()
		var out, diag bytes.Buffer
		if !binaryMode {
			code := run(t.Context(), args, &out, &diag)
			return code, out.String(), diag.String()
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), "GRAYLING_GROWTH_TOKEN=CREDENTIAL_SECRET", "OTHER_SECRET=CREDENTIAL_SECRET")
		cmd.Stdout = &out
		cmd.Stderr = &diag
		code := 0
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatal(err)
			}
			code = ee.ExitCode()
		}
		return code, out.String(), diag.String()
	}
	for _, binaryMode := range []bool{false, true} {
		mode := "parser"
		if binaryMode {
			mode = "binary"
		}
		for _, command := range commands {
			for _, name := range strings.Fields(command.flags) {
				for _, conflict := range []bool{false, true} {
					first, second := "PRIVATE_FIRST_SECRET", "PRIVATE_FIRST_SECRET"
					if name == "timeout" {
						first = "10s"
						second = first
					}
					if name == "base-url" {
						first = srv.URL
						second = first
					}
					if conflict {
						second = "PRIVATE_SECOND_SECRET"
						if name == "timeout" {
							second = "20s"
						}
						if name == "base-url" {
							second = srv.URL + "/PRIVATE_SECOND_SECRET"
						}
					}
					forms := [][]string{
						{"--" + name, first, "--" + name, second},
						{"-" + name + "=" + first, "--" + name, second},
						{"--" + name, first, "-" + name + "=" + second},
						{"--" + name + "=" + first, "-" + name + "=" + second},
					}
					for i, form := range forms {
						t.Run(mode+"/"+command.name+"/"+name+"/"+strconv.Itoa(i)+"/"+map[bool]string{true: "conflicting", false: "identical"}[conflict], func(t *testing.T) {
							args := []string{command.name}
							// Supply remaining options so scope/origin/credential
							// duplicates are tested in a complete command context.
							// Unreadable artifacts must never be opened on rejection.
							for _, other := range strings.Fields(command.flags) {
								if other == name {
									continue
								}
								value := filepath.Join(dir, "UNREADABLE_SECRET")
								switch other {
								case "base-url":
									value = srv.URL
								case "workspace":
									value = "ws1"
								case "environment":
									value = "env1"
								case "token-env":
									value = "GRAYLING_GROWTH_TOKEN"
								case "timeout":
									value = "10s"
								case "text":
									value = "literal"
								}
								args = append(args, "--"+other, value)
							}
							if command.name == "search-learning" {
								args = append(args, "--record", filepath.Join(dir, "UNREADABLE_SECRET"))
							}
							code, out, diag := execute(t, append(args, form...), binaryMode)
							if code != 2 || out != "" || diag != "invalid flags; use --help\n" {
								t.Fatalf("code=%d out=%q diagnostics=%q", code, out, diag)
							}
						})
					}
				}
			}
			t.Run(mode+"/"+command.name+"/help", func(t *testing.T) {
				code, out, diag := execute(t, []string{command.name, "--help"}, binaryMode)
				if command.name == "fingerprint-goal" {
					if code != 2 || out != "" || diag != "invalid flags; use --help\n" {
						t.Fatalf("help: %d %q %q", code, out, diag)
					}
				} else if code != 0 || out != usage || diag != "" {
					t.Fatalf("help: %d %q %q", code, out, diag)
				}
			})
		}
		// Mixed record spellings stay repeatable; a flag-looking text is a value.
		r := exportedLearningFixture(t)
		r.OriginalHypothesis = "--workspace"
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "record.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"search-learning", "--workspace", "ws1", "--environment", "env1", "--text", "--workspace", "--record", path, "-record=" + path, "--record=" + path}
		code, out, diag := execute(t, args, binaryMode)
		var result growth.LearningSearchResult
		if code != 0 || diag != "" || json.Unmarshal([]byte(out), &result) != nil || len(result.Matches) != 1 {
			t.Fatalf("repeatable control: %d %s %s", code, out, diag)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("rejected inputs made %d HTTP requests", requests.Load())
	}
	// Confirm the request counter observes a valid read command and the
	// existing authorization diagnostic/GET-only transport still work.
	proposal, err := json.Marshal(fixtureProposal(t))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "proposal.json")
	if err := os.WriteFile(path, proposal, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAYLING_GROWTH_TOKEN", "CREDENTIAL_SECRET")
	for _, binaryMode := range []bool{false, true} {
		before := requests.Load()
		code, out, diag := execute(t, []string{"review", "--proposal", path, "--base-url", srv.URL, "--workspace", "ws1", "--environment", "env1"}, binaryMode)
		if code != 1 || out != "" || diag != "evidence access denied\n" || requests.Load() <= before {
			t.Fatalf("valid GET control: %d %q %q requests=%d", code, out, diag, requests.Load()-before)
		}
	}
	if nonGET.Load() != 0 {
		t.Fatalf("made %d non-GET requests", nonGET.Load())
	}

}
