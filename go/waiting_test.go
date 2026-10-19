package job

import (
	"encoding/json"
	"os"
	"testing"
)

// Every client prints the same sentence for the same catalogue word
// (JOB-A15). The OpenCode adopter and the ComfyUI card helper test against
// the same rows.
func TestWaitingSentence(t *testing.T) {
	data, err := os.ReadFile("../testdata/waiting-sentences.json")
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Sentences []struct{ Word, Sentence string }
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatal(err)
	}
	if len(table.Sentences) == 0 {
		t.Fatal("testdata/waiting-sentences.json has no rows")
	}
	for _, row := range table.Sentences {
		if got := WaitingSentence(row.Word); got != row.Sentence {
			t.Errorf("WaitingSentence(%q) = %q, want %q", row.Word, got, row.Sentence)
		}
	}
}
