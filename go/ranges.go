package job

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// A checkpoint of ranges.
//
// # What one integer could not say
//
// A checkpoint used to hold a single verified prefix: "the first N bytes are
// proven", and nothing else. Every transfer that could be described was
// therefore one stream appending to the end of a file. That is not how bytes
// are fetched — sixteen concurrent ranged parts landing at scattered offsets in
// a sparse file is ordinary — and "parts 0, 2 and 5 done, 1, 3 and 4 partway"
// had no representation at all. An adopter with a parallel fetcher could only
// take this library by deleting its parallelism.
//
// A range set says it. The prefix is the degenerate case: one range starting at
// zero.
//
//	"checkpoint": {
//	  "verified_prefix": 4194304,
//	  "verified": [[0, 4194304], [8388608, 12582912]]
//	}
//
// # Why the prefix stays
//
// It is not redundancy and it is not politeness. A reader that has never heard
// of `verified` resumes from `verified_prefix` and re-fetches the rest, which
// is exactly what it does today. That is the whole reason this is an addition
// rather than a break, and it is why ModelRanges is never critical: an old
// reader that ignores the ranges loses some bytes to a second fetch, and
// marking it critical would stop every existing reader dead for no safety gain.
//
// The prefix is therefore never wrong in the other direction either. It is
// DERIVED from the set on write — the end of the range starting at zero, or 0
// when there is none — so nothing has to remember to keep the two agreeing.
//
// # What a range means
//
// Proven, not merely written. A range belongs in the set when its bytes are on
// disk AND checked — against a piece digest where the kind's spec carries one,
// against the transport's own framing where it does not. Bytes in flight when a
// process is killed are exactly the ones a successor must not trust; that rule
// is unchanged, and now applies per range instead of to one tail.
//
// # Where this sits
//
// A checkpoint is opaque to this package: Kind says who may read it, and
// nothing here branches on what is inside one. These helpers do not change
// that. They are a canonical FORM offered to whoever writes a checkpoint, not a
// meaning this layer reads out of every record it relays. Encode leaves a
// checkpoint exactly as it found it; only a caller that asks for ranges gets
// them rewritten.
//
// ModelRanges is named for the download kind and lives here for the same reason
// the form does: three languages have to spell the same state identically, and
// this is the package all three of them share.

// ModelRanges is the content model for a checkpoint carrying proven ranges.
//
// NEVER critical. See the note above: an old reader ignoring it re-fetches
// bytes, which costs time; refusing it costs every existing reader.
const ModelRanges = "abstraction.download/ranges@1"

// carried is the models this layer declares but cannot derive, because what
// they describe lives inside a field that is opaque here.
//
// Everything else in Content is rediscovered on every write, so a declaration
// cannot drift from the data. A model describing the CHECKPOINT's contents
// cannot be: working it out would mean reading the checkpoint, and this package
// does not know what a checkpoint is. So the declaration is carried instead —
// the same rule an unknown extension gets, for the same reason. A caller that
// writes ranges declares the model; a caller that stops writing them must clear
// it, which is what ClearCheckpointRanges is for.
var carried = map[string]bool{ModelRanges: true}

// neverCritical is the models that must not appear in Critical whoever asked.
//
// Both are advisory by definition, and a record that marks one critical is
// telling a stranger to refuse work over a decoration. Critical is otherwise
// preserved as the caller left it — an extension's writer is entitled to say
// "refuse this if you cannot read my payload" — but not for these two, because
// their own definition says a reader ignoring them is still correct.
var neverCritical = map[string]bool{ModelStep: true, ModelRanges: true}

// Range is a half-open byte interval: Start is included, End is not. An empty
// range (Start == End) proves nothing and is dropped from a canonical set.
type Range struct {
	Start int64
	End   int64
}

func (r Range) Len() int64  { return r.End - r.Start }
func (r Range) Empty() bool { return r.End <= r.Start }

func (r Range) String() string {
	return "[" + strconv.FormatInt(r.Start, 10) + "," + strconv.FormatInt(r.End, 10) + ")"
}

// Ranges is a set of proven byte ranges in canonical form: sorted by start,
// non-overlapping, non-adjacent, and containing no empty range.
//
// Adjacent ranges are merged as well as overlapping ones, and that is not
// tidiness. [[0,4],[4,8]] and [[0,8]] are the same set of proven bytes; if both
// were legal, two implementations could write the same state as different bytes
// and a conformance test comparing files would call them a disagreement.
// Merging touching ranges is what makes the form canonical rather than merely
// sorted.
type Ranges []Range

// CanonicalRanges sorts, merges and validates a set of ranges.
//
// This is the merge-on-write the format promises. Callers hand in whatever they
// have — out of order, overlapping, duplicated, adjacent — and get the one
// spelling of that state which every implementation agrees on.
func CanonicalRanges(in []Range) (Ranges, error) {
	kept := make([]Range, 0, len(in))
	for _, r := range in {
		if r.Start < 0 || r.End < 0 {
			return nil, fmt.Errorf("%w: a byte offset cannot be negative: %s", ErrInvalid, r)
		}
		if r.End < r.Start {
			return nil, fmt.Errorf("%w: a range ends before it starts: %s", ErrInvalid, r)
		}
		// An empty range is not an error — a fetcher that recorded a zero-length
		// part is not lying, it has just proven nothing — but it carries no
		// information and two sets differing only by one are the same set.
		if r.End == r.Start {
			continue
		}
		kept = append(kept, r)
	}
	sort.Slice(kept, func(i, j int) bool {
		if kept[i].Start != kept[j].Start {
			return kept[i].Start < kept[j].Start
		}
		return kept[i].End < kept[j].End
	})

	out := make(Ranges, 0, len(kept))
	for _, r := range kept {
		if n := len(out); n > 0 && r.Start <= out[n-1].End {
			if r.End > out[n-1].End {
				out[n-1].End = r.End
			}
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// VerifiedPrefix is the end of the range starting at zero, or 0 when there is
// none. In a canonical set at most one range can start at zero, and it is the
// first, so this is the whole rule.
func (rs Ranges) VerifiedPrefix() int64 {
	if len(rs) > 0 && rs[0].Start == 0 {
		return rs[0].End
	}
	return 0
}

// Add returns the set with one more proven range folded in, canonically.
func (rs Ranges) Add(start, end int64) (Ranges, error) {
	return CanonicalRanges(append(append(make([]Range, 0, len(rs)+1), rs...), Range{start, end}))
}

// Union returns the two sets merged, canonically.
func (rs Ranges) Union(other Ranges) (Ranges, error) {
	return CanonicalRanges(append(append(make([]Range, 0, len(rs)+len(other)), rs...), other...))
}

// Covers reports whether every byte of [start, end) is proven. An empty
// interval is covered by anything, including the empty set.
func (rs Ranges) Covers(start, end int64) bool {
	if end <= start {
		return true
	}
	for _, r := range rs {
		if r.Start <= start && end <= r.End {
			return true
		}
	}
	return false
}

// Missing returns the gaps in [start, end) that are not yet proven — what a
// fetcher still has to ask for. Canonical, in the same shape as Ranges.
func (rs Ranges) Missing(start, end int64) Ranges {
	out := make(Ranges, 0, len(rs)+1)
	at := start
	for _, r := range rs {
		if r.End <= at {
			continue
		}
		if r.Start >= end {
			break
		}
		if r.Start > at {
			out = append(out, Range{at, r.Start})
		}
		if r.End > at {
			at = r.End
		}
		if at >= end {
			break
		}
	}
	if at < end {
		out = append(out, Range{at, end})
	}
	return out
}

// Total is how many proven bytes the set holds.
func (rs Ranges) Total() int64 {
	var n int64
	for _, r := range rs {
		n += r.Len()
	}
	return n
}

func (rs Ranges) Equal(other Ranges) bool {
	if len(rs) != len(other) {
		return false
	}
	for i := range rs {
		if rs[i] != other[i] {
			return false
		}
	}
	return true
}

// checkpointRangeKeys are the two keys these helpers own. Every other key in a
// checkpoint belongs to whoever wrote it and is carried through untouched.
const (
	keyVerifiedPrefix = "verified_prefix"
	keyVerified       = "verified"
)

// RangesFromCheckpoint reads a range set out of a checkpoint.
//
// Three inputs, one answer:
//
//   - `verified` present: those ranges, canonicalised.
//   - only `verified_prefix`: [[0, prefix)], because a prefix IS a range. This
//     is what lets a record written before ranges existed be read as one, and
//     what makes "the prefix is the degenerate case" true in code rather than
//     only in prose.
//   - neither, or no checkpoint at all: the empty set.
//
// When both are present the prefix is UNIONED IN rather than checked against
// the ranges. A prefix-only writer that took the job over and advanced the
// prefix without touching `verified` left a record where the two disagree, and
// the union is the only reading that loses nothing: both fields are claims that
// bytes are proven, and neither is a claim that other bytes are not.
func RangesFromCheckpoint(raw []byte) (Ranges, error) {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return Ranges{}, nil
	}
	var cp struct {
		VerifiedPrefix *int64     `json:"verified_prefix"`
		Verified       *[][]int64 `json:"verified"`
	}
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fmt.Errorf("%w: checkpoint ranges: %v", ErrInvalid, err)
	}

	in := make([]Range, 0, 8)
	if cp.Verified != nil {
		for _, pair := range *cp.Verified {
			if len(pair) != 2 {
				return nil, fmt.Errorf("%w: a verified range is a pair [start, end), got %d values", ErrInvalid, len(pair))
			}
			in = append(in, Range{pair[0], pair[1]})
		}
	}
	if cp.VerifiedPrefix != nil && *cp.VerifiedPrefix > 0 {
		in = append(in, Range{0, *cp.VerifiedPrefix})
	}
	return CanonicalRanges(in)
}

// CheckpointWithRanges writes ranges into a checkpoint, canonically,
// keeping every key it does not own.
//
// The form is pinned, because three implementations have to produce the same
// bytes for the same state:
//
//	{"verified_prefix":P,"verified":[[s,e],...],<everything else, by key>}
//
// The two range keys come first and in that order — a reader skimming a record
// should see the number that matters first — and the caller's other keys follow
// sorted by name. Sorted rather than left as found because Go reaches a
// checkpoint through a map, which has no order to preserve, so "as found" is
// not a thing all three languages can agree to do.
func CheckpointWithRanges(raw []byte, rs Ranges) ([]byte, error) {
	canon, err := CanonicalRanges(rs)
	if err != nil {
		return nil, err
	}

	others := map[string]json.RawMessage{}
	if len(raw) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &others); err != nil {
			return nil, fmt.Errorf("%w: a checkpoint carrying ranges must be a JSON object: %v", ErrInvalid, err)
		}
	}
	delete(others, keyVerifiedPrefix)
	delete(others, keyVerified)
	names := make([]string, 0, len(others))
	for name := range others {
		names = append(names, name)
	}
	sort.Strings(names)

	var b bytes.Buffer
	b.WriteString(`{"` + keyVerifiedPrefix + `":`)
	b.WriteString(strconv.FormatInt(canon.VerifiedPrefix(), 10))
	b.WriteString(`,"` + keyVerified + `":[`)
	for i, r := range canon {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		b.WriteString(strconv.FormatInt(r.Start, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(r.End, 10))
		b.WriteByte(']')
	}
	b.WriteByte(']')
	for _, name := range names {
		key, err := json.Marshal(name)
		if err != nil {
			return nil, err
		}
		b.WriteByte(',')
		b.Write(key)
		b.WriteByte(':')
		if err := json.Compact(&b, others[name]); err != nil {
			return nil, fmt.Errorf("%w: checkpoint key %q: %v", ErrInvalid, name, err)
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// CheckpointRanges is the proven ranges this record's checkpoint carries.
//
// A record that has never checkpointed, and one whose checkpoint predates
// ranges entirely, both answer without an error: the first with the empty set,
// the second with the prefix as one range.
func (r *Record) CheckpointRanges() (Ranges, error) {
	return RangesFromCheckpoint(r.Checkpoint)
}

// SetCheckpointRanges records what is proven, merged into canonical form, and
// declares the ranges model.
//
// Both halves matter. Without the canonical form two writers spell one state
// two ways; without the declaration a reader cannot tell whether an absent
// `verified` means "nothing proven beyond the prefix" or "this writer had never
// heard of ranges".
func (r *Record) SetCheckpointRanges(rs []Range) error {
	raw, err := CheckpointWithRanges(r.Checkpoint, Ranges(rs))
	if err != nil {
		return err
	}
	r.Checkpoint = raw
	if !contains(r.Content, ModelRanges) {
		r.Content = append(r.Content, ModelRanges)
	}
	return nil
}

// AddCheckpointRange folds one newly proven range into the checkpoint. This is
// what a parallel fetcher calls as each part lands.
func (r *Record) AddCheckpointRange(start, end int64) error {
	have, err := r.CheckpointRanges()
	if err != nil {
		return err
	}
	next, err := have.Add(start, end)
	if err != nil {
		return err
	}
	return r.SetCheckpointRanges(next)
}

// ClearCheckpointRanges removes the ranges and the declaration, leaving every
// other key in the checkpoint alone.
//
// The declaration is carried rather than derived, so it has to be withdrawn
// explicitly; a record that kept declaring a model whose data it no longer
// holds would be telling a reader to look for something that is not there.
func (r *Record) ClearCheckpointRanges() error {
	others := map[string]json.RawMessage{}
	if len(r.Checkpoint) > 0 && string(bytes.TrimSpace(r.Checkpoint)) != "null" {
		if err := json.Unmarshal(r.Checkpoint, &others); err != nil {
			return fmt.Errorf("%w: checkpoint is not a JSON object: %v", ErrInvalid, err)
		}
	}
	delete(others, keyVerified)
	if len(others) == 0 {
		r.Checkpoint = nil
	} else {
		names := make([]string, 0, len(others))
		for name := range others {
			names = append(names, name)
		}
		sort.Strings(names)
		var b bytes.Buffer
		b.WriteByte('{')
		for i, name := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			key, err := json.Marshal(name)
			if err != nil {
				return err
			}
			b.Write(key)
			b.WriteByte(':')
			if err := json.Compact(&b, others[name]); err != nil {
				return fmt.Errorf("%w: checkpoint key %q: %v", ErrInvalid, name, err)
			}
		}
		b.WriteByte('}')
		r.Checkpoint = b.Bytes()
	}
	kept := r.Content[:0]
	for _, name := range r.Content {
		if name != ModelRanges {
			kept = append(kept, name)
		}
	}
	r.Content = kept
	return nil
}
