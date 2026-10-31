package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/saker-ai/grayling/internal/growth"
)

func runInitProposal(args []string, out, diagnostics io.Writer) int {
	fail := func(code int, message string) int {
		fmt.Fprintln(diagnostics, message)
		return code
	}
	fs := flag.NewFlagSet("init-proposal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("goal", "", "caller-attested goal file")
	id := fs.String("proposal-id", "", "new proposal ID")
	versionText := fs.String("proposal-version", "", "positive decimal int64 version")
	workspace := fs.String("workspace", "", "independent operator workspace ID")
	environment := fs.String("environment", "", "independent operator environment ID")
	if err := parseFlags(fs, args); err != nil {
		return fail(2, "invalid flags; use --help")
	}
	version, err := strconv.ParseInt(*versionText, 10, 64)
	if fs.NArg() != 0 || *path == "" || err != nil || version < 1 {
		return fail(2, "invalid initialization configuration")
	}
	goal, err := readGoal(*path)
	if err != nil {
		return fail(2, "invalid goal file or JSON")
	}
	proposal, err := growth.InitProposal(*goal, *id, version, *workspace, *environment)
	if err != nil {
		return fail(2, "invalid proposal initialization input")
	}
	if err := writeJSONArtifact(out, proposal); err != nil {
		return fail(1, "output failed")
	}
	return 0
}
