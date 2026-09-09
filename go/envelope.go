package job

import (
	"errors"
	"fmt"
	"strings"
)

// The base envelope, and the two questions it answers that `kind` cannot:
// which schema a job's opaque halves follow, and what may be asked of a job of
// that kind. A supervisor that does not know the kind could previously find an
// orphan and reclaim it, and nothing else.
//
// NOTHING IN THIS FILE, OR IN THIS PACKAGE, RESOLVES A SCHEMA IDENTIFIER.
// There is no function here from a schema name to a schema, in any binding; the
// only questions askable of one are whether it is well formed and whether it is
// a string the caller already knows. That is the whole safety argument, and the
// grammar below is the half that makes it structural rather than a promise: a
// legal identifier cannot be a URL, a UNC name or a path, so a supervisor that
// wanted to fetch one would first have to be handed something ValidSchema
// refuses. A supervisor that fetches an address out of somebody else's record
// is running attacker-chosen content as a service on the owner's machine.

// ErrNotSupported is an action a kind does not declare, or one this supervisor
// has not implemented. The Verdict vocabulary in job.thrift has carried the
// word since it was written and nothing in Go stood for it.
var ErrNotSupported = errors.New("job: action not supported")

// BaseSchema is the schema a bare action name belongs to. The bare namespace is
// this layer's; anyone else's name is qualified, or two vendors both define
// "cancel" and a supervisor cannot tell which one it just performed.
const BaseSchema = FeatureBase

// FeatureEnvelope is that the record says which schema its opaque halves follow
// and what may be asked of them. Critical whenever present: a reader that
// ignored it would carry on and then write the record back without it, deleting
// the kind's own declaration, and the thing destroyed would be the description
// of what was destroyed.
const FeatureEnvelope = "abstraction.job/envelope@1"

// The base action vocabulary: what may be asked of ANY job through this layer's
// own operations, and which a kind declares it actually honours.
//
// Capability says what an IMPLEMENTATION promises. This says what a KIND
// honours, and they are different questions — a store that survives process
// exit still cannot pause a kind whose worker has no pause, and a pause button
// that silently does nothing is worse than no pause button. All four are asks,
// never instructions: each names an existing Store operation, and none of them
// can describe how to carry anything out.
const (
	ActionPause  = "pause"  // set_intent pause is honoured
	ActionResume = "resume" // set_intent run after a pause is honoured
	ActionCancel = "cancel" // set_intent cancel is honoured
	ActionRecall = "recall" // the holder yields the lease when recalled
)

var baseActions = map[string]bool{
	ActionPause:  true,
	ActionResume: true,
	ActionCancel: true,
	ActionRecall: true,
}

// BaseActions is the bare vocabulary, sorted, so a harness can diff one
// implementation's roster against another's.
func BaseActions() []string {
	return []string{ActionCancel, ActionPause, ActionRecall, ActionResume}
}

// Envelope is what a job of this kind is and what may be asked of it.
//
// Two fields and no third. An envelope is a property of the KIND, so nothing in
// it may report what is true of this instance now: a supervisor that read
// "pause" here and concluded this job can be paused at this moment would be
// reading live state off a static declaration, and the job's worker may have
// died an hour ago. The stores refuse an update that moves it.
type Envelope struct {
	// Schema names the schema `spec` and `checkpoint` follow. A NAME, never an
	// address — see the note at the top of this file.
	Schema string `json:"schema"`

	// Actions are names a supervisor matches against what it already
	// implements. Never anything executable, and nothing here can say how to do
	// something: matching is the only operation defined on them.
	Actions []string `json:"actions,omitempty"`
}

const (
	maxSchema = 128
	maxLabel  = 63
	maxAction = 64
)

func lowerAlnum(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }

// label is one dot-separated part of a schema identifier: lower-case letters,
// digits and interior hyphens. Deliberately the smallest alphabet that can name
// anything, because everything it leaves out is what a URL, a path and a
// command line are made of.
func label(s string) bool {
	if s == "" || len(s) > maxLabel {
		return false
	}
	if !lowerAlnum(s[0]) || !lowerAlnum(s[len(s)-1]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !lowerAlnum(s[i]) && s[i] != '-' {
			return false
		}
	}
	return true
}

func version(s string) bool {
	if s == "" || len(s) > 9 || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// ValidSchema reports whether s is a schema identifier [JOB-V1]:
//
//	namespace "/" name "@" version
//
// spelled exactly as this record's content names already are, so there is one
// grammar for names here and not two. There is no scheme, no authority, no
// percent-escape, no backslash, no query and exactly one "/" and one "@", which
// is what makes "https://example.invalid/x@1", "\\host\share", "../../etc" and
// "file:///x" all illegal values rather than merely discouraged ones.
func ValidSchema(s string) bool {
	if s == "" || len(s) > maxSchema {
		return false
	}
	if strings.Count(s, "/") != 1 || strings.Count(s, "@") != 1 {
		return false
	}
	slash := strings.IndexByte(s, '/')
	at := strings.IndexByte(s, '@')
	if at < slash {
		return false
	}
	for _, part := range strings.Split(s[:slash], ".") {
		if !label(part) {
			return false
		}
	}
	return label(s[slash+1:at]) && version(s[at+1:])
}

func bareAction(s string) bool {
	if s == "" || len(s) > maxAction {
		return false
	}
	if !lowerAlnum(s[0]) || !lowerAlnum(s[len(s)-1]) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !lowerAlnum(s[i]) && s[i] != '-' && s[i] != '_' {
			return false
		}
	}
	return true
}

// SplitAction separates an action name into the schema that declares it and the
// bare name [JOB-V2]. A bare name returns "" for the schema, which means the
// base schema.
//
// The separator is "#" because it is the one component of a URI reference that
// is defined never to be sent anywhere (RFC 3986 §3.5): a fragment names
// something inside a document rather than a document to go and get. It is also
// excluded from both grammars either side of it, so the split is exact.
func SplitAction(name string) (schema, bare string, ok bool) {
	switch strings.Count(name, "#") {
	case 0:
		return "", name, bareAction(name)
	case 1:
		i := strings.IndexByte(name, '#')
		schema, bare = name[:i], name[i+1:]
		return schema, bare, ValidSchema(schema) && bareAction(bare)
	}
	return "", "", false
}

// validate checks the envelope against the rules no schema language has a place
// for [JOB-V1] [JOB-V2] [JOB-V3].
func (e *Envelope) validate() error {
	if !ValidSchema(e.Schema) {
		return fmt.Errorf("%w: %q is not a schema identifier; a schema is named, never fetched", ErrInvalid, e.Schema)
	}
	seen := make(map[string]bool, len(e.Actions))
	for _, name := range e.Actions {
		if seen[name] {
			return fmt.Errorf("%w: action %q is declared twice", ErrInvalid, name)
		}
		seen[name] = true
		schema, bare, ok := SplitAction(name)
		if !ok {
			return fmt.Errorf("%w: %q is not an action name", ErrInvalid, name)
		}
		switch {
		case schema == "":
			// The bare namespace is this layer's. A kind that could put its own
			// word here would be squatting a name a later version of this layer
			// may define, and the supervisor would perform the wrong one.
			if !baseActions[bare] {
				return fmt.Errorf("%w: %q is bare and is not one of this layer's actions; a kind's own action carries the schema that declares it", ErrInvalid, name)
			}
		case schema == BaseSchema:
			// One spelling of one action, so that Supports is a comparison and
			// not a negotiation.
			return fmt.Errorf("%w: %q spells a base action the long way; write it bare", ErrInvalid, name)
		case schema != e.Schema:
			// Every action name resolves to a schema THIS RECORD declares.
			// Without it a record could name actions in a namespace it has no
			// claim to, and a supervisor matching on the name alone would act
			// on somebody else's authority.
			return fmt.Errorf("%w: action %q names a schema this record does not declare (%q)", ErrInvalid, name, e.Schema)
		}
	}
	return nil
}

func (e *Envelope) clone() *Envelope {
	if e == nil {
		return nil
	}
	c := *e
	c.Actions = append([]string(nil), e.Actions...)
	return &c
}

func sameEnvelope(a, b *Envelope) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Schema != b.Schema || len(a.Actions) != len(b.Actions) {
		return false
	}
	for i := range a.Actions {
		if a.Actions[i] != b.Actions[i] {
			return false
		}
	}
	return true
}

// envelopeUnmoved enforces [JOB-V4]: the envelope is a property of the KIND, so
// a lease holder cannot move it.
//
// The service binding gets this for free — it enumerates the fields a write may
// carry and the envelope is not among them — but the in-process bindings run the
// caller's own closure against the record, so here the rule has to be checked
// rather than arranged. Without it "supported action" becomes live state by the
// ordinary route: somebody sets it from a worker that has just found something
// out, and every supervisor afterwards reads a fact about one process as a fact
// about the kind.
func envelopeUnmoved(before, after *Envelope) error {
	if sameEnvelope(before, after) {
		return nil
	}
	return fmt.Errorf("%w: the envelope is a property of the kind and is written once, at submit", ErrInvalid)
}

// Schema is the schema this job's opaque halves follow, or "" when the record
// does not say. A name to compare, not a place to look.
func (r *Record) Schema() string {
	if r.Envelope == nil {
		return ""
	}
	return r.Envelope.Schema
}

// Actions are the action names the kind declares, in the record's own order.
func (r *Record) Actions() []string {
	if r.Envelope == nil {
		return nil
	}
	return append([]string(nil), r.Envelope.Actions...)
}

// Supports reports whether the KIND declares this action. Not whether it can be
// performed now: this is a static declaration, and the worker may have died an
// hour ago.
//
// A bare name and a qualified one are different actions and never match each
// other, which is the point of qualifying at all.
func (r *Record) Supports(action string) bool {
	if r.Envelope == nil {
		return false
	}
	for _, name := range r.Envelope.Actions {
		if name == action {
			return true
		}
	}
	return false
}

// A Supervisor is what a caller has actually built, in its own process: the
// schemas it was written against and the actions it implements.
//
// It is a value the caller constructs and never something read out of a record.
// That is the shape the whole design turns on — an unknown schema is answered
// from this list, so the answer to "I have never heard of this" is a refusal and
// there is nowhere for it to be a fetch.
type Supervisor struct {
	Schemas []string
	Actions []string
}

// Ask reports whether this supervisor may perform action on this record.
//
// It performs nothing. Every refusal names what was refused, because a
// supervisor that silently declines is indistinguishable from one that quietly
// did the wrong thing:
//
//	ErrInvalid        the name is not an action name
//	ErrUnknownSchema  the record follows a schema this supervisor does not know
//	ErrNotSupported   the kind does not declare it, or this supervisor has not
//	                  implemented it
func (r *Record) Ask(action string, s Supervisor) error {
	schema, _, ok := SplitAction(action)
	if !ok {
		return fmt.Errorf("%w: %q is not an action name", ErrInvalid, action)
	}
	if r.Envelope == nil {
		return fmt.Errorf("%w: %s declares no envelope, so it declares no actions", ErrNotSupported, r.ID)
	}
	if !contains(s.Schemas, r.Envelope.Schema) {
		return fmt.Errorf("%w: %q, which this supervisor was not written against", ErrUnknownSchema, r.Envelope.Schema)
	}
	if schema != "" && !contains(s.Schemas, schema) {
		return fmt.Errorf("%w: %q, which this supervisor was not written against", ErrUnknownSchema, schema)
	}
	if !r.Supports(action) {
		return fmt.Errorf("%w: %s does not declare %q", ErrNotSupported, r.Kind, action)
	}
	if !contains(s.Actions, action) {
		return fmt.Errorf("%w: this supervisor does not implement %q", ErrNotSupported, action)
	}
	return nil
}
