package growth

import (
	"os"
	"strings"
	"testing"
)

func TestOwnedFiniteSamplesAndImmutablePins(t *testing.T) {
	for _, name := range []string{"activation", "upgrade"} {
		file, e := os.Open("../../examples/growth-synthetic/" + name + "-baseline.json")
		if e != nil {
			t.Fatal(e)
		}
		b, e := DecodeAggregateBaseline(file)
		file.Close()
		if e != nil || !b.Synthetic || b.Causal {
			t.Fatal("invalid owned finite sample")
		}
		pin, e := PinBaseline(*b)
		if e != nil {
			t.Fatal(e)
		}
		before := pin.Fingerprint
		b.Groups[0].Converted++
		after, e := BaselineFingerprint(*b)
		if e != nil || before == after {
			t.Fatal("counts absent from pin")
		}
		b.Causal = true
		if ValidateAggregateBaseline(*b) == nil {
			t.Fatal("observational source claims causality")
		}
	}
	for _, raw := range []string{"null", `{"rawUsers":["forbidden"]}`, strings.Repeat(" ", 65537)} {
		if _, e := DecodeAggregateBaseline(strings.NewReader(raw)); e == nil {
			t.Fatal("unsafe finite input")
		}
	}
}
