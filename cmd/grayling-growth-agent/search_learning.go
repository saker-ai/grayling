package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/saker-ai/grayling/internal/growth"
)

func readLearningRecord(path string, remaining int64) (*growth.LearningRecord, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, growth.ErrLearning
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > growth.MaxLearningRecordBytes || info.Size() > remaining {
		return nil, 0, growth.ErrLearning
	}
	// Count actual bytes as well as stat size so growth during reading cannot
	// bypass either bound. O_NONBLOCK prevents a FIFO from blocking on open.
	data, err := io.ReadAll(io.LimitReader(f, min(int64(growth.MaxLearningRecordBytes), remaining)+1))
	if err != nil || int64(len(data)) > remaining || len(data) > growth.MaxLearningRecordBytes {
		return nil, 0, growth.ErrLearning
	}
	r, err := growth.DecodeLearningRecord(bytes.NewReader(data))
	return r, int64(len(data)), err
}

func runSearchLearning(args []string, out, diagnostics io.Writer) int {
	fail := func(message string) int { fmt.Fprintln(diagnostics, message); return 2 }
	fs := flag.NewFlagSet("search-learning", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	workspace := fs.String("workspace", "", "required workspace")
	environment := fs.String("environment", "", "required environment")
	text := fs.String("text", "", "case-sensitive literal hypothesis/follow-up substring")
	goalFile := fs.String("goal", "", "optional caller-attested goal file for exact metric-role matching")
	var paths []string
	fs.Func("record", "exported learning record file (repeatable)", func(s string) error {
		if s == "" || len(paths) >= growth.MaxLearningSearchFiles {
			return growth.ErrLearning
		}
		paths = append(paths, s)
		return nil
	})
	if err := parseFlags(fs, args, "record"); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(out, usage)
			return 0
		}
		return fail("invalid flags; use --help")
	}
	q := growth.LearningSearchQuery{WorkspaceID: *workspace, EnvironmentID: *environment, Text: *text}
	if fs.NArg() != 0 || len(paths) == 0 || q.Validate() != nil {
		return fail("invalid search configuration")
	}
	var goal *growth.GrowthGoal
	goalSupplied := false
	fs.Visit(func(f *flag.Flag) { goalSupplied = goalSupplied || f.Name == "goal" })
	if goalSupplied {
		var err error
		goal, err = readGoal(*goalFile)
		if err != nil {
			return fail("invalid goal file or JSON")
		}
		if goal.WorkspaceID != q.WorkspaceID || goal.EnvironmentID != q.EnvironmentID {
			return fail("invalid or inconsistent goal search inputs")
		}
	}
	records := make([]growth.LearningRecord, 0, len(paths))
	remaining := int64(growth.MaxLearningSearchBytes)
	for _, path := range paths {
		r, n, err := readLearningRecord(path, remaining)
		if err != nil {
			return fail("invalid learning record file or JSON")
		}
		remaining -= n
		records = append(records, *r)
	}
	var result any
	var err error
	if goal != nil {
		result, err = growth.SearchGoalLearning(*goal, records, q)
	} else {
		result, err = growth.SearchLearning(records, q)
	}
	if err != nil {
		return fail("invalid learning records")
	}
	if err := writeJSONArtifact(out, result); err != nil {
		fmt.Fprintln(diagnostics, "output failed")
		return 1
	}
	return 0
}
