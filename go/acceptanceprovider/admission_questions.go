package acceptanceprovider

import (
	"bytes"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// QuestionAdmissionChecker runs an executor's admission check in
// questions-only mode [JOB-A16]. Submit asks it when the caller's
// acceptance.submit decision for the kind reads not_granted and its first-use
// question was raised or joined, so the person sees every question the first
// submission needs together (backlog RIGHTS-FLOW, amendment "One wave of
// questions").
//
// It answers, in order, the ids of the first-use questions admission raised
// or joined that are still pending about the binding's own program, limited
// to the targets asks permission@1 Request serves (ASK-A14), and nothing
// else: no outcome, no reason, no credential word, no missing grant without a
// question. A caller without acceptance.submit learns only what Request would
// already tell it. It applies no credential and has no effect on the job
// store; the provider journals, accepts and seals nothing for the refusal.
type QuestionAdmissionChecker interface {
	AdmissionQuestions(binding Binding, kind string, spec []byte, required []string) []string
}

const (
	notGrantedWord      = "rights:not_granted"
	questionPendingWord = "rights:question_pending:"
	maxRefusalReason    = 1024
)

// submitQuestionPending reports a method policy's Submit refusal for a
// not_granted decision whose first-use question was raised or joined: its
// first word is rights:not_granted and it names a question.
func submitQuestionPending(reason string) bool {
	words := strings.Fields(reason)
	if len(words) < 2 || words[0] != notGrantedWord {
		return false
	}
	return slices.ContainsFunc(words[1:], func(w string) bool {
		return strings.HasPrefix(w, questionPendingWord) && len(w) > len(questionPendingWord)
	})
}

// executorGuarantees are the required guarantees beyond the job owner's own
// vocabulary, which the executor's admission is asked about.
func executorGuarantees(required []string) []string {
	extra := []string{}
	for _, g := range required {
		if !slices.Contains(Guarantees(), g) {
			extra = append(extra, g)
		}
	}
	return extra
}

// nameAdmissionQuestions appends to a Submit refusal for a not_granted submit
// decision with its question asked one rights:question_pending:<id> word for
// each question the executor's questions-only admission raised or joined
// about the caller's own program [JOB-A16]. The refusal's own words come
// first and stay whole within the reason's 1024 bytes; a question that would
// not fit is left out. Any other refusal, a submission the provider would not
// admit to the executor, and an executor without the mode keep reason as it
// is. It reads no journal and writes nothing.
func (b *bound) nameAdmissionQuestions(reason string, s api.Submission) string {
	p := b.provider
	checker, ok := p.executor.(QuestionAdmissionChecker)
	if !ok || b.scope == "" || b.binding.Subject == nil || !submitQuestionPending(reason) || len(reason) > maxRefusalReason {
		return reason
	}
	if validateSpec(s) != nil {
		return reason
	}
	if _, err := p.acceptedGuarantees(s.RequiredGuarantees); err != nil {
		return reason
	}
	ids := checker.AdmissionQuestions(b.binding, s.Kind, bytes.Clone(s.Spec), executorGuarantees(s.RequiredGuarantees))
	var named []string
	for _, w := range strings.Fields(reason) {
		if id, ok := strings.CutPrefix(w, questionPendingWord); ok {
			named = append(named, id)
		}
	}
	for _, id := range ids {
		if id == "" || !utf8.ValidString(id) || strings.ContainsFunc(id, unicode.IsSpace) || slices.Contains(named, id) {
			continue
		}
		word := " " + questionPendingWord + id
		if len(reason)+len(word) > maxRefusalReason {
			break
		}
		named = append(named, id)
		reason += word
	}
	return reason
}
