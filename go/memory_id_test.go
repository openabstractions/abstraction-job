package job

import (
	"errors"
	"testing"
)

// The memory store refuses the same record ids the file store refuses.
func TestMemoryStoreSubmitRefusesAnIDThatIsNotOneFile(t *testing.T) {
	m := NewMemoryStore()
	for _, id := range []string{"../escape", `a\b`, "a/b", ".."} {
		if _, err := m.Submit(Record{ID: id}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Submit(%q) = %v, want ErrInvalid", id, err)
		}
	}
	if _, err := m.Submit(Record{ID: "fine", Kind: "test", Spec: []byte(`{}`)}); err != nil {
		t.Fatalf("Submit(fine) = %v", err)
	}
}
