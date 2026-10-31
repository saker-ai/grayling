package main

import (
	"errors"
	"flag"
)

// parseFlags makes registered options singleton unless explicitly repeatable.
// Wrapping Set delegates tokenization to flag: a flag-looking string consumed
// as a value is never mistaken for an option, and help/terminators keep their
// standard ordering. Callers discard parser errors and emit fixed diagnostics.
func parseFlags(fs *flag.FlagSet, args []string, repeatable ...string) error {
	originals := make(map[*flag.Flag]flag.Value)
	fs.VisitAll(func(f *flag.Flag) {
		for _, name := range repeatable {
			if f.Name == name {
				return
			}
		}
		originals[f] = f.Value
		f.Value = &singletonValue{Value: f.Value}
	})
	defer func() {
		for f, value := range originals {
			f.Value = value
		}
	}()
	return fs.Parse(args)
}

type singletonValue struct {
	flag.Value
	seen bool
}

func (v *singletonValue) Set(value string) error {
	if v.seen {
		return errors.New("repeated singleton flag")
	}
	v.seen = true
	return v.Value.Set(value)
}

func (v *singletonValue) IsBoolFlag() bool {
	b, ok := v.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
