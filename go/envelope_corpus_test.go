package job

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The cross-language corpus for the envelope, and the reason it is files rather
// than three sets of table-driven tests.
//
// Three implementations transcribed from one author agree about the unwritten
// parts by descent. The rules the envelope adds — the schema grammar, and that
// every action name resolves to a schema the record declares — are reader
// obligations, which the definition cannot state and the generated codec
// therefore cannot enforce. So they are the exact class of rule where three
// hand-written readers drift silently. One directory of records, one expected
// verdict per file taken from the file's own name, read by Go, Python and C++.
//
// Set ABSTRACTION_WRITE_CORPUS=1 to rewrite the accept cases from this
// implementation; the check below is what makes that safe to do.
const corpusDir = "../testdata/envelope"

func corpusFiles(t *testing.T) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(corpusDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatalf("no corpus in %s", corpusDir)
	}
	sort.Strings(names)
	return names
}

func expectedWord(path string) string {
	return strings.SplitN(filepath.Base(path), ".", 2)[0]
}

func TestTheEnvelopeCorpusIsRefusedTheSameWayEverywhere(t *testing.T) {
	for _, path := range corpusFiles(t) {
		in, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		got, decoded := verdictFor(in)
		want := expectedWord(path)
		if got != want {
			t.Errorf("%s: expected %s, got %s", filepath.Base(path), want, got)
			continue
		}
		if want != "accept" {
			continue
		}
		// An accepted case is also the byte proof: the file IS the canonical
		// form, so a reader that re-spells anything on the way out fails here
		// rather than in a diff between two languages a week later.
		back, err := decoded.Encode()
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(path), err)
			continue
		}
		if string(back) != string(in) {
			t.Errorf("%s: decode then encode changed the bytes:\nwant %s\ngot  %s", filepath.Base(path), in, back)
		}
	}
}

func verdictFor(in []byte) (string, *Record) {
	r, err := Decode(in)
	switch {
	case err == nil:
		return "accept", r
	case errors.Is(err, ErrUnknownSchema):
		return "unknown_schema", nil
	case errors.Is(err, ErrInvalid):
		return "invalid", nil
	}
	return "other", nil
}

// asks.tsv is the half a record file cannot carry: what a supervisor holding a
// particular set of schemas and actions is told when it asks.
func TestTheAskCorpusAnswersTheSameWayEverywhere(t *testing.T) {
	f, err := os.Open(filepath.Join(corpusDir, "asks.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	n := 0
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		col := strings.Split(text, "\t")
		if len(col) != 5 {
			t.Fatalf("asks.tsv:%d: 5 columns, got %d", line, len(col))
		}
		in, err := os.ReadFile(filepath.Join(corpusDir, col[0]))
		if err != nil {
			t.Fatal(err)
		}
		r, err := Decode(in)
		if err != nil {
			t.Fatalf("asks.tsv:%d: %v", line, err)
		}
		s := Supervisor{Schemas: fields(col[2]), Actions: fields(col[3])}
		got := askWord(r.Ask(col[1], s))
		if got != col[4] {
			t.Errorf("asks.tsv:%d: %s asked %q: expected %s, got %s", line, col[0], col[1], col[4], got)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("asks.tsv holds no cases")
	}
}

func fields(s string) []string {
	if s == "-" {
		return nil
	}
	return strings.Fields(s)
}

func askWord(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrUnknownSchema):
		return "unknown_schema"
	case errors.Is(err, ErrNotSupported):
		return "not_supported"
	case errors.Is(err, ErrInvalid):
		return "invalid"
	}
	return "other"
}
