package job

import (
	"errors"
	"strings"
	"testing"

	"github.com/openabstractions/abstraction-job/go/rec"
)

func historicalTerminalFixture(state string, marked bool) string {
	raw := strings.Replace(generatedRecordFixture, `"state":"running"`, `"state":"`+state+`"`, 1)
	if marked {
		raw = strings.Replace(raw, `"content":["abstraction.job/base@1"]`, `"content":["abstraction.job/base@1","abstraction.job/terminal@1"],"critical":["abstraction.job/base@1","abstraction.job/terminal@1"]`, 1)
	} else {
		raw = strings.Replace(raw, `"content":["abstraction.job/base@1"]`, `"content":["abstraction.job/base@1"],"critical":["abstraction.job/base@1"]`, 1)
	}
	return raw
}

func TestNativeHistoricalTerminalReadPreservesMeaningAndDeclaration(t *testing.T) {
	for _, state := range []string{"complete", "failed", "cancelled"} {
		for _, marked := range []bool{false, true} {
			raw := historicalTerminalFixture(state, marked)
			wire, wireErr := rec.Decode([]byte(raw))
			if (wireErr == nil) != marked {
				t.Fatalf("public derived language changed: state=%s marked=%v record=%+v err=%v", state, marked, wire, wireErr)
			}
			r, err := Decode([]byte(raw))
			if err != nil {
				t.Fatalf("historical state=%s marked=%v: %v", state, marked, err)
			}
			if string(r.State) != state || !r.State.Terminal() || contains(r.Content, FeatureTerminal) != marked || contains(r.Critical, FeatureTerminal) != marked {
				t.Fatalf("historical declaration changed: %+v", r)
			}
			encoded, err := r.Encode()
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := rec.Decode(encoded)
			if err != nil || canonical.State != state || !contains(canonical.Content, FeatureTerminal) || !contains(canonical.Critical, FeatureTerminal) {
				t.Fatalf("new writer lost terminal fence: %+v %v", canonical, err)
			}
		}
	}
}

func TestNativeHistoricalTerminalReadKeepsRefusalBoundaries(t *testing.T) {
	base := historicalTerminalFixture("failed", false)
	cases := map[string]string{
		"duplicate native":          strings.Replace(base, `"id":"job"`, `"id":"job","i\u0064":"other"`, 1),
		"duplicate opaque":          strings.Replace(base, `{"what":"anything"}`, `{"a":1,"a":2}`, 1),
		"deep opaque":               strings.Replace(base, `{"what":"anything"}`, strings.Repeat("[", 65)+"0"+strings.Repeat("]", 65), 1),
		"wrong type":                strings.Replace(base, `"state":"failed"`, `"state":42`, 1),
		"unknown field":             strings.Replace(base, `"kind":"test"`, `"kind":"test","future":true`, 1),
		"unknown critical":          strings.Replace(base, `"critical":["abstraction.job/base@1"]`, `"critical":["future/required@1"]`, 1),
		"undeclared critical":       strings.Replace(base, `"critical":["abstraction.job/base@1"]`, `"critical":["abstraction.job/terminal@1"]`, 1),
		"other missing declaration": strings.Replace(base, `"kind":"test"`, `"kind":"test","intent":{"want":"pause","by":"operator"}`, 1),
		"missing base":              strings.Replace(base, `"content":["abstraction.job/base@1"]`, `"content":[]`, 1),
		"wrong state declaration":   historicalTerminalFixture("running", true),
		"invalid native state":      historicalTerminalFixture("future", false),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatal("historical compatibility accepted malformed or unknown work")
			}
		})
	}
	if _, err := Decode([]byte(cases["unknown critical"])); !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("unknown critical changed class: %v", err)
	}
}
