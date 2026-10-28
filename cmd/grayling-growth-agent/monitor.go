package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/saker-ai/grayling/internal/growth"
)

func runMonitor(ctx context.Context, args []string, out, diagnostics io.Writer) int {
	fail := func(code int, message string) int { fmt.Fprintln(diagnostics, message); return code }
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("manifest", "", "operator manifest")
	base := fs.String("base-url", "", "trusted API origin")
	workspace := fs.String("workspace", "", "trusted workspace")
	environment := fs.String("environment", "", "trusted environment")
	tokenEnv := fs.String("token-env", "GRAYLING_GROWTH_TOKEN", "credential environment variable")
	timeout := fs.Duration("timeout", 10*time.Second, "total read timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return 0
		}
		return fail(2, "invalid flags; use --help")
	}
	if fs.NArg() != 0 || *file == "" || *timeout <= 0 || *timeout > 30*time.Second || !envName.MatchString(*tokenEnv) {
		return fail(2, "invalid configuration")
	}
	m, err := readMonitorManifest(*file)
	if err != nil {
		return fail(2, "invalid monitor manifest")
	}
	if m.WorkspaceID != *workspace || m.EnvironmentID != *environment {
		return fail(2, "monitor configuration scope mismatch")
	}
	reader, err := growth.NewReader(growth.Config{BaseURL: *base, WorkspaceID: *workspace, EnvironmentID: *environment, BearerToken: os.Getenv(*tokenEnv), Timeout: *timeout})
	if err != nil {
		return fail(2, "invalid configuration")
	}
	defer reader.Close()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	summary, err := reader.Monitor(ctx, *m)
	if err != nil {
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return fail(130, "monitor canceled")
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fail(1, "evidence deadline exceeded")
		case errors.Is(err, growth.ErrAuthorization):
			return fail(1, "evidence access denied")
		case errors.Is(err, growth.ErrMismatch):
			return fail(1, "evidence scope or version mismatch")
		default:
			return fail(1, "evidence execution failed")
		}
	}
	if err := json.MarshalWrite(out, summary); err != nil {
		return fail(1, "output failed")
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return fail(1, "output failed")
	}
	if summary.Status != growth.Ready {
		return 3
	}
	return 0
}

func readMonitorManifest(path string) (*growth.MonitorManifest, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, growth.ErrConfig
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > growth.MaxMonitorManifestBytes {
		return nil, growth.ErrConfig
	}
	return growth.DecodeMonitorManifest(f)
}
