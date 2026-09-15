package acceptanceprovider

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// The journal shape of providers released before attempts and lost results,
// decoded as they decode it: strictly, refusing unknown fields.
type preAttemptIdentity struct{ Key, HistoryEpoch string }
type preAttemptSubmission struct {
	Identity           preAttemptIdentity
	Kind               string
	Spec               []byte
	RequiredGuarantees []string
}
type preAttemptReceipt struct {
	Identity           preAttemptIdentity
	LogicalOwner       string
	OperationId        string
	AcceptedGuarantees []string
	HistoryRetentionMs int64
}
type preAttemptJournal struct {
	Version      int
	Scope        string
	Identity     preAttemptIdentity
	Phase        string
	Arguments    *preAttemptSubmission
	Receipt      *preAttemptReceipt
	Reason       string
	WorkSpec     []byte   `json:",omitempty"`
	WorkRequires []string `json:",omitempty"`
	Origin       string   `json:",omitempty"`
}

func TestJournalsStayReadableByPreAttemptProviders(t *testing.T) {
	p := openTest(t, t.TempDir())
	c := p.Bind("alice")
	original := submission(p, "compat")
	first := accept(t, c, original)
	sealed := submission(p, "compat-sealed")
	if v, err := c.Reconcile(sealed.Identity); err != nil || v.Outcome != "definitely_not_accepted" {
		t.Fatalf("seal: %+v %v", v, err)
	}
	journalBytes := func(id api.RequestIdentity) []byte {
		t.Helper()
		data, err := os.ReadFile(p.requestPath("alice", id))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, id := range []api.RequestIdentity{original.Identity, sealed.Identity} {
		data := journalBytes(id)
		var old preAttemptJournal
		if err := strict(data, &old); err != nil {
			t.Fatalf("an older provider refuses an attempt-zero journal: %v\n%s", err, data)
		}
		// Byte-for-byte the document an older provider writes for the same journal.
		again, err := json.Marshal(old)
		if err != nil || !bytes.Equal(again, data) {
			t.Fatalf("attempt-zero journal differs from the pre-attempt encoding:\n%s\n%s", data, again)
		}
	}

	endOperation(t, p, first.OperationId, job.StateFailed)
	retried := retry(original, 1)
	accept(t, c, retried)
	var old preAttemptJournal
	if err := strict(journalBytes(retried.Identity), &old); err == nil {
		t.Fatal("an older provider decoded a retry journal it cannot honour")
	}
	j, _, err := p.readJournal("alice", retried.Identity)
	if err != nil || j.Identity.Attempt != 1 || j.Arguments.Identity.Attempt != 1 || j.Receipt.Identity.Attempt != 1 {
		t.Fatalf("retry journal round trip: %+v %v", j, err)
	}

	// Unknown fields stay refused by the custom decoder.
	var decoded journal
	if err := strict([]byte(`{"Version":1,"Scope":"alice","Identity":{"Key":"k","HistoryEpoch":"e"},"Phase":"sealed","Arguments":null,"Receipt":null,"Reason":"","Future":1}`), &decoded); err == nil {
		t.Fatal("journal decoder granted an unknown field")
	}
}
