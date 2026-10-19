package job

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

const generatedRecordFixture = `{"content":["abstraction.job/base@1"],"id":"job","kind":"test","state":"running","spec":{"what":"anything"},"progress":{"done":0,"updated_at":"2026-01-01T00:00:00.000000Z"},"lease":{"owner":"","epoch":0,"expires_at":"2026-01-01T00:00:00.000000Z"},"created_at":"2026-01-01T00:00:00.000000Z","updated_at":"2026-01-01T00:00:00.000000Z"}`

func TestGeneratedRecordHistoricalProfiles(t *testing.T) {
	for _, schema := range []string{"3", "4", "5"} {
		t.Run(schema, func(t *testing.T) {
			raw := strings.Replace(generatedRecordFixture, `"content":["abstraction.job/base@1"]`, `"schema":`+schema, 1)
			r, err := Decode([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if !contains(r.Content, FeatureBase) {
				t.Fatal(r.Content)
			}
			if schema == "5" && !contains(r.Content, FeatureStep) {
				t.Fatal(r.Content)
			}
			encoded, err := r.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte(`"schema":`)) {
				t.Fatal("legacy discriminator was written")
			}
			if _, err := Decode(encoded); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGeneratedLegacyMappingReplacesCriticalOnlyWithEmptyContent(t *testing.T) {
	for _, schema := range []string{"3", "4", "5"} {
		for _, delegated := range []bool{false, true} {
			raw := strings.Replace(generatedRecordFixture, `"content":["abstraction.job/base@1"]`, `"schema":`+schema+`,"critical":["example/unknown@1"]`, 1)
			if delegated {
				raw = strings.Replace(raw, `"state":"running"`, `"state":"running","delegation":{"system":"test","external_id":"owned"}`, 1)
			}
			r, err := Decode([]byte(raw))
			if err != nil {
				t.Fatalf("schema %s delegation %v: %v", schema, delegated, err)
			}
			if contains(r.Critical, "example/unknown@1") || !contains(r.Critical, FeatureBase) || contains(r.Critical, FeatureDelegation) != delegated {
				t.Fatalf("schema %s delegation %v: %v", schema, delegated, r.Critical)
			}
		}
		present := strings.Replace(generatedRecordFixture, `"content":`, `"schema":`+schema+`,"critical":["example/unknown@1"],"content":`, 1)
		if _, err := Decode([]byte(present)); !errors.Is(err, ErrUnknownSchema) {
			t.Fatalf("content-present critical was replaced: %v", err)
		}
	}
}

func TestGeneratedProposalRetainsPreSubmitShape(t *testing.T) {
	for _, raw := range []string{`{}`, `{"kind":"test","spec":{"x":1}}`, strings.Replace(generatedRecordFixture, `"id":"job",`, ``, 1)} {
		if _, err := DecodeProposal([]byte(raw)); err != nil {
			t.Fatalf("proposal %s: %v", raw, err)
		}
		if _, err := Decode([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("complete record %s: %v", raw, err)
		}
	}
	proposal := &Record{Kind: "test", Spec: []byte(`{"x":1}`)}
	b, err := proposal.EncodeProposal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeProposal(b); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(b); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsubmitted proposal became complete: %v", err)
	}
}

func TestGeneratedRecordNativeStepWidthRefusesBeforeWrite(t *testing.T) {
	if strconv.IntSize == 32 {
		t.Skip("native int already has the IDL width")
	}
	r, err := Decode([]byte(generatedRecordFixture))
	if err != nil {
		t.Fatal(err)
	}
	tooWide := int64(1) << 31
	r.Progress.Step = &Step{Name: "phase", Ordinal: int(tooWide)}
	if _, err := r.Encode(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("out-of-range native ordinal: %v", err)
	}
}

func TestGeneratedRecordRejectsMalformedNativeAcceptance(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(generatedRecordFixture, `"created_at":"2026-01-01T00:00:00.000000Z"`, `"created_at":null`, 1),
		strings.ReplaceAll(generatedRecordFixture, `2026-01-01T00:00:00.000000Z`, ``),
		strings.Replace(generatedRecordFixture, `"id":"job"`, `"id":"\ud800"`, 1),
		strings.Replace(generatedRecordFixture, `"id":"job"`, `"id":"job","i\u0064":"other"`, 1),
	} {
		if _, err := Decode([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted malformed record: %s: %v", raw, err)
		}
	}
}

func TestGeneratedProposalSharesNestedGrammar(t *testing.T) {
	for _, raw := range []string{`{"progress":null}`, `{"lease":null}`, `{"intent":null}`, `{"progress":{"done":0}}`, `{"extensions":{"x":{"a":1,"a":2}}}`, `{"new":true}`, `{"id":"one","i\u0064":"two"}`} {
		if _, err := DecodeProposal([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("malformed proposal %s: %v", raw, err)
		}
	}
	if r, err := DecodeProposal([]byte(`{"id":"\ud83d\ude00"}`)); err != nil || r.ID != "😀" {
		t.Fatalf("valid paired Unicode: %v, %v", r, err)
	}
}

// JOB-J7/J8 preserve opaque token spelling while reindenting whitespace. J6's
// owned-string escaping does not rewrite an opaque author's Unicode tokens.
func TestGeneratedRecordOpaqueUnicodeTokensStayVerbatim(t *testing.T) {
	for _, token := range []string{"\"\u2028\u2029\"", `"\u2028\u2029"`, `"<>&"`, `123456789012345678901234567890`} {
		r, err := Decode([]byte(generatedRecordFixture))
		if err != nil {
			t.Fatal(err)
		}
		r.Spec = []byte(`{"value":` + token + `}`)
		if err := r.SetSpec(json.RawMessage(r.Spec)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(r.Spec, []byte(token)) {
			t.Fatalf("native payload conversion changed token %q: %s", token, r.Spec)
		}
		encoded, err := r.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(encoded, []byte(token)) {
			t.Fatalf("opaque token %q changed: %s", token, encoded)
		}
		again, err := Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		twice, err := again.Encode()
		if err != nil || !bytes.Equal(encoded, twice) {
			t.Fatalf("round trip changed tokens: %v", err)
		}
	}
}
