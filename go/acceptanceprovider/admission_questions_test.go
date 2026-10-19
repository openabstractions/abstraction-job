package acceptanceprovider

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// questionExecutor answers questions-only admission with ids and full
// admission with a refusal that carries every word questions-only mode drops.
type questionExecutor struct {
	testExecutor
	ids      []string
	asked    *int
	full     *int
	bindings *[]Binding
}

func (e questionExecutor) CheckAdmission(scope, kind string, spec []byte, required []string) (api.AcceptanceOutcome, string) {
	*e.full++
	return api.AcceptanceOutcomeInvalid, "credential:unknown:hf host:router:no_host"
}

func (e questionExecutor) CheckSubjectAdmission(binding Binding, kind string, spec []byte, required []string) (api.AcceptanceOutcome, string) {
	*e.full++
	return api.AcceptanceOutcomeInvalid, "credential:unknown:hf host:router:no_host"
}

func (e questionExecutor) AdmissionQuestions(binding Binding, kind string, spec []byte, required []string) []string {
	*e.asked++
	*e.bindings = append(*e.bindings, binding)
	return slices.Clone(e.ids)
}

func newQuestionExecutor(ids ...string) questionExecutor {
	asked, full := 0, 0
	return questionExecutor{testExecutor: testExecutor{profile: "questions-v1", prefix: "private/"}, ids: ids, asked: &asked, full: &full, bindings: &[]Binding{}}
}

// boundSubmit is the job service's Submit for a bound program whose method
// policy refused with refusal, as HandleConnectionWithBindingPolicy composes
// it: refusal is the policy's PermissionRefusalReason, or "" for a refusal
// without one.
func boundSubmit(t *testing.T, p *Provider, granted access, refusal string, s api.Submission) api.AcceptanceResult {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "agent.exe")
	scope, err := LocalSubjectScope("windows", "S-1-5-21-1", executable)
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{Scope: scope, Origin: "local", Subject: &AuthenticatedSubject{AccountKind: "windows", Account: "S-1-5-21-1", Executable: executable, Program: executable}}
	if binding.valid() != nil {
		t.Fatal(binding.valid())
	}
	h := methodAcceptance{allowed: p.BindBinding(binding), denied: p.Bind(""),
		permit:        func(string, string, string) access { return granted },
		refusalReason: func() string { return refusal }}
	r, err := h.Submit(s)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func pictureSubmission(p *Provider, key string) api.Submission {
	return api.Submission{Identity: api.RequestIdentity{Key: key, HistoryEpoch: p.config.Epoch}, Kind: "inference", Spec: []byte(`{"prompt":"frogs"}`), RequiredGuarantees: []string{GuaranteeServiceRestart}}
}

// TestFreshCallerHearsEveryQuestionInOneRefusal: a program with no submit,
// host or output rules submits its first picture job. Its acceptance.submit
// decision reads not_granted and asks; the job service then runs the
// executor's admission in questions-only mode, and the one forbidden refusal
// names the submit question, the host question and the output-pair question,
// grant words first. Nothing is journaled (JOB-A16, backlog RIGHTS-FLOW
// amendment "One wave of questions"). J-PIC saw the host and output questions
// only after the submit Allow and a spent retry on 2026-11-10.
func TestFreshCallerHearsEveryQuestionInOneRefusal(t *testing.T) {
	root := t.TempDir()
	e := newQuestionExecutor("q-host", "q-output")
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	before := storageTree(t, root)
	r := boundSubmit(t, p, accessDenied, "rights:not_granted rights:question_pending:q-submit", pictureSubmission(p, "first-picture"))
	want := "rights:not_granted rights:question_pending:q-submit rights:question_pending:q-host rights:question_pending:q-output"
	if r.Outcome != api.AcceptanceOutcomeForbidden || r.Reason != want || r.Receipt != nil {
		t.Fatalf("first picture refusal %s %q, want %q", r.Outcome, r.Reason, want)
	}
	if *e.asked != 1 || *e.full != 0 {
		t.Fatalf("questions-only admission ran %d times, full admission %d", *e.asked, *e.full)
	}
	if b := (*e.bindings)[0]; b.Subject == nil || b.Subject.Program == "" {
		t.Fatalf("questions-only admission lost the caller's program: %+v", b)
	}
	if !reflect.DeepEqual(before, storageTree(t, root)) || jobs(t, root) != 0 {
		t.Fatal("a refused submission journaled, accepted or sealed")
	}
}

// TestRefusedSubmitWithoutAQuestionRunsNoAdmission: a stored Block on
// acceptance.submit, an unavailable decision and a not_granted decision whose
// question cannot be asked return as before. The executor's admission does
// not run and raises no host or output question.
func TestRefusedSubmitWithoutAQuestionRunsNoAdmission(t *testing.T) {
	root := t.TempDir()
	e := newQuestionExecutor("q-host", "q-output")
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	before := storageTree(t, root)
	s := pictureSubmission(p, "blocked")
	for name, c := range map[string]struct {
		granted access
		refusal string
		want    api.AcceptanceOutcome
	}{
		"stored Block":           {accessDenied, "", api.AcceptanceOutcomeForbidden},
		"decision unavailable":   {accessUnavailable, "", api.AcceptanceOutcomeUnavailable},
		"question not askable":   {accessDenied, "rights:not_granted", api.AcceptanceOutcomeForbidden},
		"another refusal's word": {accessDenied, "rights:denied rights:question_pending:q-submit", api.AcceptanceOutcomeForbidden},
	} {
		r := boundSubmit(t, p, c.granted, c.refusal, s)
		if r.Outcome != c.want || strings.Contains(r.Reason, "q-host") || strings.Contains(r.Reason, "q-output") {
			t.Fatalf("%s: %s %q", name, r.Outcome, r.Reason)
		}
	}
	if *e.asked != 0 || *e.full != 0 {
		t.Fatalf("a refusal without a submit question ran admission: questions-only %d, full %d", *e.asked, *e.full)
	}
	if !reflect.DeepEqual(before, storageTree(t, root)) || jobs(t, root) != 0 {
		t.Fatal("a refused submission journaled, accepted or sealed")
	}
}

// TestQuestionsOnlyAdmissionDropsEveryOtherReason: the caller without
// acceptance.submit hears question ids only. The executor's full refusal
// (invalid, a credential word, an unknown host) never reaches it, nor an id
// that is empty, carries whitespace or repeats the submit question. Words
// that would pass the 1024-byte reason are left out, and the grant words stay
// whole. A permitted submit still receives admission's full reasons.
func TestQuestionsOnlyAdmissionDropsEveryOtherReason(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("x", 400)
	e := newQuestionExecutor("", "q-submit", "two words", "q\ttab", "q-host", "q-host", long+"1", long+"2", long+"3")
	p, err := OpenWithExecutor(root, "owner", e)
	if err != nil {
		t.Fatal(err)
	}
	r := boundSubmit(t, p, accessDenied, "rights:not_granted rights:question_pending:q-submit", pictureSubmission(p, "dropped"))
	want := "rights:not_granted rights:question_pending:q-submit rights:question_pending:q-host rights:question_pending:" + long + "1 rights:question_pending:" + long + "2"
	if r.Outcome != api.AcceptanceOutcomeForbidden || r.Reason != want || len(r.Reason) > 1024 {
		t.Fatalf("questions-only refusal %q (%d bytes), want %q", r.Reason, len(r.Reason), want)
	}
	for _, word := range strings.Fields(r.Reason)[1:] {
		if !strings.HasPrefix(word, "rights:question_pending:") {
			t.Fatalf("questions-only refusal carries %q", word)
		}
	}
	if strings.Contains(r.Reason, "invalid") || strings.Contains(r.Reason, "no_host") || *e.full != 0 {
		t.Fatalf("a full admission reason reached the caller: %q (full admission %d)", r.Reason, *e.full)
	}
	// A submission the provider would never hand to the executor asks nothing.
	unsupported := pictureSubmission(p, "unsupported")
	unsupported.RequiredGuarantees = append(unsupported.RequiredGuarantees, "example.execution/never@1")
	asked := *e.asked
	if r := boundSubmit(t, p, accessDenied, "rights:not_granted rights:question_pending:q-submit", unsupported); r.Reason != "rights:not_granted rights:question_pending:q-submit" || *e.asked != asked {
		t.Fatalf("unsupported guarantee: %q asked %d", r.Reason, *e.asked-asked)
	}
	// A permitted submit runs admission as before, with its full reasons.
	if r := boundSubmit(t, p, accessAllowed, "", pictureSubmission(p, "permitted")); r.Outcome != api.AcceptanceOutcomeInvalid || r.Reason != "credential:unknown:hf host:router:no_host" || *e.full != 1 {
		t.Fatalf("permitted submit: %s %q full %d", r.Outcome, r.Reason, *e.full)
	}
}
