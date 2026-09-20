package acceptance

import (
	"strings"
	"testing"
)

// TestNormalizeLabel covers the JOB-A12 limits: trimming, absence, length in
// bytes, and the one-line printable rule.
func TestNormalizeLabel(t *testing.T) {
	long := strings.Repeat("é", MaxLabelBytes/2)
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"   \t ", "", true},
		{"  huggingface.co · model.safetensors \n", "huggingface.co · model.safetensors", true},
		{long, long, true},
		{long + "x", "", false},
		{"a\x00b", "", false},
		{"a\nb", "", false},
		{"a\x7fb", "", false},
		{"a b", "", false},
		{"a b", "", false},
		{"a\xffb", "", false},
	} {
		got, err := NormalizeLabel(c.in)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("NormalizeLabel(%q) = %q, %v; want %q, ok=%v", c.in, got, err, c.want, c.ok)
		}
	}
	s := Submission{Identity: RequestIdentity{Key: "k", HistoryEpoch: "e"}, Kind: "download", Label: "a\x01"}
	if ValidateSubmission(s) == nil {
		t.Fatal("a control character in the label validated")
	}
}
