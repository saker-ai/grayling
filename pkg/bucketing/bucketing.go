// Package bucketing implements the GoLab stable-bucketing specification
// golab-fnv1a-10000-v1 (docs/技术方案.md §6).
//
// # Specification (normative)
//
// All inputs are strings. epoch is the decimal ASCII representation of a
// non-negative integer. Every field is encoded as a netstring:
//
//	netstring(s) = ASCII_decimal(len(UTF-8(s))) ":" UTF-8(s)
//
// The bucketing input is the concatenation, in this exact order:
//
//	netstring(workspace) + netstring(environment) + netstring(key) +
//	netstring(epoch) + netstring(salt) + netstring(unitValue) + netstring(purpose)
//
// where key is the experiment ID when the flag is bound to an experiment,
// otherwise the flag key. The input is hashed with FNV-1a 64-bit
// (offset basis 14695981039346656037, prime 1099511628211) and the bucket is
//
//	bucket = hash % 10000        // range [0, 9999]
//
// purpose separates independent hash coordinates. Reserved purposes:
//
//	"eligibility" — experiment enrollment decision
//	"variant"     — arm assignment inside an enrolled population
//	"layer"       — mutual-exclusion layer slot assignment
//
// Missing/empty fields are encoded as "0:" (they are never omitted). Every
// string field must be valid UTF-8 (invalid sequences are rejected — they do
// not survive JSON transport across languages); no Unicode normalization is
// applied, so callers must pass the unit value verbatim (pseudonymization
// policy lives above this package).
//
// The fixed modulus keeps hash coordinates stable but does NOT keep variant
// assignment stable across ratio changes: shrinking/growing a split moves
// range boundaries. Cross-ratio stability requires sticky records or a new
// epoch (docs §6); nothing in this package promises otherwise.
//
// testdata/golden_vectors.json is the cross-language contract: any re-
// implementation MUST reproduce every vector.
package bucketing

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf8"
)

// AlgorithmID is the version tag stored in flag bucketing configs.
const AlgorithmID = "golab-fnv1a-10000-v1"

// Buckets is the fixed modulus of the v1 specification.
const Buckets = 10000

// Reserved hash purposes.
const (
	PurposeEligibility = "eligibility"
	PurposeVariant     = "variant"
	PurposeLayer       = "layer"
)

// Inputs carries the seven normative fields of one bucketing computation.
type Inputs struct {
	Workspace   string
	Environment string
	// Key is the experiment ID for experiment-bound flags, else the flag key.
	Key string
	// Epoch is >= 0; changing salt/algorithm/unit requires a new epoch (§6).
	Epoch int64
	Salt  string
	// UnitValue is the stable randomization-unit identifier, verbatim.
	UnitValue string
	// Purpose separates independent hash coordinates (see package doc).
	Purpose string
}

// Validate rejects inputs that would make the vector non-portable.
//
// All string fields MUST be valid UTF-8: invalid byte sequences do not
// survive JSON transport or re-encoding in other languages, so they are
// rejected here instead of silently hashing to implementation-specific
// values.
func (in Inputs) Validate() error {
	if in.Epoch < 0 {
		return fmt.Errorf("bucketing: negative epoch %d", in.Epoch)
	}
	if in.Purpose == "" {
		return fmt.Errorf("bucketing: empty purpose")
	}
	if strings.ContainsRune(in.Purpose, ':') {
		return fmt.Errorf("bucketing: purpose must not contain ':'")
	}
	for name, field := range map[string]string{
		"workspace": in.Workspace, "environment": in.Environment,
		"key": in.Key, "salt": in.Salt, "unitValue": in.UnitValue,
		"purpose": in.Purpose,
	} {
		if !utf8.ValidString(field) {
			return fmt.Errorf("bucketing: field %q is not valid UTF-8", name)
		}
	}
	return nil
}

// CanonicalInput renders the exact byte string that gets hashed. Exported for
// cross-language implementers and golden-vector tooling; production callers
// should use Bucket.
func (in Inputs) CanonicalInput() (string, error) {
	if err := in.Validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, field := range []string{
		in.Workspace,
		in.Environment,
		in.Key,
		strconv.FormatInt(in.Epoch, 10),
		in.Salt,
		in.UnitValue,
		in.Purpose,
	} {
		b.WriteString(strconv.Itoa(len(field)))
		b.WriteByte(':')
		b.WriteString(field)
	}
	return b.String(), nil
}

// Hash returns the raw FNV-1a 64-bit digest of the canonical input.
func (in Inputs) Hash() (uint64, error) {
	canon, err := in.CanonicalInput()
	if err != nil {
		return 0, err
	}
	h := fnv.New64a()
	// fnv's Write never returns an error.
	_, _ = h.Write([]byte(canon))
	return h.Sum64(), nil
}

// Bucket returns the stable bucket in [0, 9999] for the given inputs.
func Bucket(in Inputs) (int, error) {
	hash, err := in.Hash()
	if err != nil {
		return 0, err
	}
	return int(hash % Buckets), nil
}

// MustBucket is Bucket for contexts where inputs are already validated
// (e.g. static test tables). It panics on invalid inputs.
func MustBucket(in Inputs) int {
	b, err := Bucket(in)
	if err != nil {
		panic(err)
	}
	return b
}
