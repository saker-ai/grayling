package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

func runCheckGoal(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	fail := func(code int, message string) int { fmt.Fprintln(diagnostics, message); return code }
	fs := flag.NewFlagSet("check-goal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	goalFile := fs.String("goal", "", "caller-attested goal file")
	file := fs.String("manifest", "", "operator manifest")
	base := fs.String("base-url", "", "trusted API origin")
	workspace := fs.String("workspace", "", "trusted workspace")
	environment := fs.String("environment", "", "trusted environment")
	tokenEnv := fs.String("token-env", "GRAYLING_GROWTH_TOKEN", "credential environment variable")
	timeout := fs.Duration("timeout", 10*time.Second, "total read timeout")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return 0
		}
		return fail(2, "invalid flags; use --help")
	}
	if fs.NArg() != 0 || *file == "" || *goalFile == "" || *timeout <= 0 || *timeout > 30*time.Second || !envName.MatchString(*tokenEnv) {
		return fail(2, "invalid configuration")
	}
	goal, err := readGoal(*goalFile)
	if err != nil {
		return fail(2, "invalid goal file or JSON")
	}
	m, err := readMonitorManifest(*file)
	if err != nil {
		return fail(2, "invalid monitor manifest")
	}
	reader, err := growth.NewReader(growth.Config{BaseURL: *base, WorkspaceID: *workspace, EnvironmentID: *environment, BearerToken: os.Getenv(*tokenEnv), Timeout: *timeout})
	if err != nil {
		return fail(2, "invalid configuration")
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	summary, err := reader.CheckGoal(ctx, *goal, *m)
	if err != nil {
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return fail(130, "check-goal canceled")
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fail(1, "evidence deadline exceeded")
		case errors.Is(err, growth.ErrGoal), errors.Is(err, growth.ErrGoalVerification), errors.Is(err, growth.ErrConfig):
			return fail(2, "invalid or inconsistent goal check inputs")
		case errors.Is(err, growth.ErrAuthorization):
			return fail(1, "evidence access denied")
		case errors.Is(err, growth.ErrMismatch):
			return fail(1, "evidence scope or version mismatch")
		default:
			return fail(1, "evidence execution failed")
		}
	}
	if err := writeJSONArtifact(out, summary); err != nil {
		return fail(1, "output failed")
	}
	if summary.Status != growth.Ready {
		return 3
	}
	return 0
}
