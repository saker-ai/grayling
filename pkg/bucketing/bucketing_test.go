package bucketing_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/saker-ai/grayling/pkg/bucketing"
)

type goldenVector struct {
	Desc        string `json:"desc"`
	Workspace   string `json:"workspace"`
	Environment string `json:"environment"`
	Key         string `json:"key"`
	Epoch       int64  `json:"epoch"`
	Salt        string `json:"salt"`
	UnitValue   string `json:"unitValue"`
	Purpose     string `json:"purpose"`
	Hash        string `json:"hash64Hex"`
	Bucket      int    `json:"bucket"`
}

type goldenFile struct {
	Algorithm string         `json:"algorithm"`
	Modulus   int            `json:"modulus"`
	Vectors   []goldenVector `json:"vectors"`
}

// TestGoldenVectors enforces the cross-language contract. The vectors were
// independently re-derived with a separate FNV-1a implementation (Python) —
// a mismatch here means the spec or this implementation drifted.
func TestGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden_vectors.json"))
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var gf goldenFile
	if err := json.Unmarshal(raw, &gf); err != nil {
		t.Fatalf("parse golden vectors: %v", err)
	}
	if gf.Algorithm != bucketing.AlgorithmID {
		t.Fatalf("golden file algorithm = %q, want %q", gf.Algorithm, bucketing.AlgorithmID)
	}
	if gf.Modulus != bucketing.Buckets {
		t.Fatalf("golden file modulus = %d, want %d", gf.Modulus, bucketing.Buckets)
	}
	if len(gf.Vectors) < 16 {
		t.Fatalf("golden file has only %d vectors; contract weakened?", len(gf.Vectors))
	}
	for _, v := range gf.Vectors {
		in := bucketing.Inputs{
			Workspace: v.Workspace, Environment: v.Environment, Key: v.Key,
			Epoch: v.Epoch, Salt: v.Salt, UnitValue: v.UnitValue, Purpose: v.Purpose,
		}
		hash, err := in.Hash()
		if err != nil {
			t.Errorf("%s: Hash error: %v", v.Desc, err)
			continue
		}
		if got := pad16(hash); got != v.Hash {
			t.Errorf("%s: hash = %s, want %s", v.Desc, got, v.Hash)
		}
		bucket := int(hash % bucketing.Buckets)
		if bucket != v.Bucket {
			t.Errorf("%s: bucket = %d, want %d", v.Desc, bucket, v.Bucket)
		}
	}
}

func pad16(h uint64) string {
	const digits = "0123456789abcdef"
	buf := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		buf[i] = digits[h&0xf]
		h >>= 4
	}
	return string(buf)
}

// TestBucketDeterministicAndDistributed: same inputs are stable, the unit
// space is roughly uniform, and independent purposes decorrelate.
func TestBucketDeterministicAndDistributed(t *testing.T) {
	base := bucketing.Inputs{
		Workspace: "ws", Environment: "prod", Key: "flag-a",
		Epoch: 0, Salt: "s1", Purpose: bucketing.PurposeVariant,
	}

	// Determinism.
	first := bucketing.MustBucket(base)
	for i := 0; i < 100; i++ {
		if got := bucketing.MustBucket(base); got != first {
			t.Fatalf("bucket not deterministic: %d then %d", first, got)
		}
	}

	// Uniformity over 100k units: every decile of the bucket space should
	// hold ~10% of units (±20% relative is a generous sanity bound).
	const n = 100_000
	counts := make([]int, 10)
	for i := 0; i < n; i++ {
		in := base
		in.UnitValue = "user-" + itoa(i)
		b := bucketing.MustBucket(in)
		if b < 0 || b >= bucketing.Buckets {
			t.Fatalf("bucket %d out of range for user-%d", b, i)
		}
		counts[b/1000]++
	}
	for decile, c := range counts {
		want := n / 10
		if math.Abs(float64(c-want)) > 0.2*float64(want) {
			t.Errorf("decile %d has %d units, want ~%d", decile, c, want)
		}
	}

	// eligibility and variant purposes must decorrelate for the same unit.
	same := 0
	for i := 0; i < 1000; i++ {
		v := base
		v.UnitValue = "u-" + itoa(i)
		e := v
		e.Purpose = bucketing.PurposeEligibility
		if bucketing.MustBucket(v) == bucketing.MustBucket(e) {
			same++
		}
	}
	if same > 50 { // ~1% expected by chance (10/1000)
		t.Errorf("eligibility and variant buckets correlate: %d/1000 equal", same)
	}
}

// TestSplitAllocation covers the deterministic largest-remainder rules.
func TestSplitAllocation(t *testing.T) {
	t.Run("90/10 exact", func(t *testing.T) {
		s, err := bucketing.NewSplit(map[string]float64{"control": 90, "candidate": 10})
		if err != nil {
			t.Fatalf("NewSplit: %v", err)
		}
		// lexicographic: candidate < control
		if names := s.Names(); names[0] != "candidate" || names[1] != "control" {
			t.Fatalf("canonical order = %v", names)
		}
		cs, ce, _ := s.Range("candidate")
		ts, te, _ := s.Range("control")
		if cs != 0 || ce != 1000 || ts != 1000 || te != 10000 {
			t.Fatalf("ranges: candidate [%d,%d) control [%d,%d)", cs, ce, ts, te)
		}
		v, err := s.Assign(999)
		if err != nil || v != "candidate" {
			t.Fatalf("Assign(999) = %v, %v", v, err)
		}
		v, err = s.Assign(1000)
		if err != nil || v != "control" {
			t.Fatalf("Assign(1000) = %v, %v", v, err)
		}
		if _, err := s.Assign(-1); err == nil {
			t.Fatal("Assign(-1) must fail")
		}
		if _, err := s.Assign(10000); err == nil {
			t.Fatal("Assign(10000) must fail")
		}
	})

	t.Run("three way fractional sums to 10000", func(t *testing.T) {
		s, err := bucketing.NewSplit(map[string]float64{"a": 33.33, "b": 33.33, "c": 33.34})
		if err != nil {
			t.Fatalf("NewSplit: %v", err)
		}
		total := 0
		for _, name := range s.Names() {
			start, end, ok := s.Range(name)
			if !ok {
				t.Fatalf("missing range for %s", name)
			}
			total += end - start
		}
		if total != bucketing.Buckets {
			t.Fatalf("ranges cover %d buckets, want %d", total, bucketing.Buckets)
		}
	})

	t.Run("equal thirds largest remainder", func(t *testing.T) {
		s, err := bucketing.NewSplit(map[string]float64{"a": 100.0 / 3, "b": 100.0 / 3, "c": 100.0 - 2*(100.0/3)})
		if err != nil {
			t.Fatalf("NewSplit: %v", err)
		}
		total := 0
		for _, name := range s.Names() {
			start, end, _ := s.Range(name)
			total += end - start
		}
		if total != bucketing.Buckets {
			t.Fatalf("ranges cover %d buckets, want %d", total, bucketing.Buckets)
		}
	})

	t.Run("zero percent gets no buckets", func(t *testing.T) {
		s, err := bucketing.NewSplit(map[string]float64{"live": 100, "dark": 0})
		if err != nil {
			t.Fatalf("NewSplit: %v", err)
		}
		start, end, _ := s.Range("dark")
		if start != end {
			t.Fatalf("dark variation got buckets [%d,%d)", start, end)
		}
		for b := 0; b < bucketing.Buckets; b++ {
			if v, _ := s.Assign(b); v == "dark" {
				t.Fatalf("bucket %d assigned to 0%% variation", b)
			}
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		cases := []map[string]float64{
			{"a": 50, "b": 49.9}, // sum != 100
			{"a": 100.0000001},   // sum != 100 within tolerance
			{"a": -1, "b": 101},  // out of range
			{},                   // empty
			{"": 100},            // empty name
			{"a": math.NaN()},    // NaN
		}
		for i, c := range cases {
			if _, err := bucketing.NewSplit(c); err == nil {
				t.Errorf("case %d (%v): expected error, got nil", i, c)
			}
		}
	})

	t.Run("single variation full range", func(t *testing.T) {
		s, err := bucketing.NewSplit(map[string]float64{"only": 100})
		if err != nil {
			t.Fatalf("NewSplit: %v", err)
		}
		start, end, _ := s.Range("only")
		if start != 0 || end != 10000 {
			t.Fatalf("range = [%d,%d)", start, end)
		}
	})
}

// TestSplitEmpiricalRatio: hash-driven assignment approximates declared
// percentages within tolerance for a realistic unit population.
func TestSplitEmpiricalRatio(t *testing.T) {
	s, err := bucketing.NewSplit(map[string]float64{"control": 90, "candidate": 10})
	if err != nil {
		t.Fatalf("NewSplit: %v", err)
	}
	const n = 50_000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		b := bucketing.MustBucket(bucketing.Inputs{
			Workspace: "ws", Environment: "prod", Key: "exp-1", Epoch: 2,
			Salt: "split-test", UnitValue: "user-" + itoa(i), Purpose: bucketing.PurposeVariant,
		})
		v, err := s.Assign(b)
		if err != nil {
			t.Fatalf("Assign(%d): %v", b, err)
		}
		counts[v]++
	}
	got := float64(counts["candidate"]) / n * 100
	if math.Abs(got-10) > 1.0 { // ±1pp on 50k samples is >3σ headroom
		t.Errorf("candidate share = %.2f%%, want ~10%%", got)
	}
}

// TestRatioChangeMovesUsers documents the spec-mandated caveat: changing the
// split ratio re-draws range boundaries, so some users necessarily move
// (docs §6 — "固定模只保证 hash 坐标不变"). Sticky records or a new epoch are
// the only ways to preserve assignments; the platform must not claim
// otherwise.
func TestRatioChangeMovesUsers(t *testing.T) {
	before, err := bucketing.NewSplit(map[string]float64{"control": 90, "candidate": 10})
	if err != nil {
		t.Fatalf("NewSplit: %v", err)
	}
	after, err := bucketing.NewSplit(map[string]float64{"control": 50, "candidate": 50})
	if err != nil {
		t.Fatalf("NewSplit: %v", err)
	}
	moved := 0
	const n = 10_000
	for i := 0; i < n; i++ {
		b := bucketing.MustBucket(bucketing.Inputs{
			Workspace: "ws", Environment: "prod", Key: "flag-a", Epoch: 0,
			Salt: "s1", UnitValue: "user-" + itoa(i), Purpose: bucketing.PurposeVariant,
		})
		v1, _ := before.Assign(b)
		v2, _ := after.Assign(b)
		if v1 != v2 {
			moved++
		}
	}
	// control shrank from [1000,10000) to [5000,10000): ~44% of users move.
	if moved == 0 {
		t.Fatal("expected some users to move when ratios change")
	}
	t.Logf("ratio change moved %d/%d users (expected ~4400)", moved, n)
}

func TestValidateRejects(t *testing.T) {
	if _, err := (bucketing.Inputs{Purpose: "variant", Epoch: -1}).Hash(); err == nil {
		t.Error("negative epoch must be rejected")
	}
	if _, err := (bucketing.Inputs{Purpose: ""}).Hash(); err == nil {
		t.Error("empty purpose must be rejected")
	}
	if _, err := (bucketing.Inputs{Purpose: "a:b"}).Hash(); err == nil {
		t.Error("purpose containing ':' must be rejected")
	}
	if _, err := (bucketing.Inputs{Purpose: "variant", UnitValue: string([]byte{0xff, 0xfe})}).Hash(); err == nil {
		t.Error("invalid UTF-8 must be rejected")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
