package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/saker-ai/grayling/internal/growth"
)

func runFingerprintGoal(args []string, out, diagnostics io.Writer) int {
	fail := func(code int, message string) int {
		fmt.Fprintln(diagnostics, message)
		return code
	}
	fs := flag.NewFlagSet("fingerprint-goal", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var path string
	goals := 0
	fs.Func("goal", "caller-attested goal file", func(value string) error {
		path = value
		goals++
		return nil
	})
	if err := parseFlags(fs, args); err != nil {
		return fail(2, "invalid flags; use --help")
	}
	if fs.NArg() != 0 || path == "" || goals != 1 {
		return fail(2, "invalid fingerprint configuration")
	}
	goal, err := readGoal(path)
	if err != nil {
		return fail(2, "invalid goal file or JSON")
	}
	fingerprint, err := growth.GoalFingerprint(*goal)
	if err != nil {
		return fail(2, "invalid goal file or JSON")
	}
	data := []byte(fingerprint + "\n")
	n, err := out.Write(data)
	if err != nil || n != len(data) {
		return fail(1, "output failed")
	}
	return 0
}
