package acceptanceprovider

import "testing"

// A notification may name only an operation its poster's scope accepted
// (ASK-N1), and the runtime posts operation_end notifications once the
// operation's record is terminal.
func TestOperationOwnedByAndEnded(t *testing.T) {
	root := t.TempDir()
	binding := testBinding(t)
	p, err := OpenWithExecutor(root, "owner", &subjectCheckingExecutor{checkingExecutor: newCheckingExecutor()})
	if err != nil {
		t.Fatal(err)
	}
	receipt := accept(t, p.BindBinding(binding), submission(p, "owned"))
	if owned, err := p.OperationOwnedBy(receipt.OperationID, binding.Scope); err != nil || !owned {
		t.Fatal("the accepting scope owns its operation", owned, err)
	}
	if owned, err := p.OperationOwnedBy(receipt.OperationID, "other-scope"); err != nil || owned {
		t.Fatal("another scope owns nothing", owned, err)
	}
	if owned, err := p.OperationOwnedBy("no-such-operation", binding.Scope); err != nil || owned {
		t.Fatal("an unknown operation is owned by nobody", owned, err)
	}
	if owned, err := p.OperationOwnedBy("Bad/ID", binding.Scope); err != nil || owned {
		t.Fatal("a malformed id is owned by nobody", owned, err)
	}
	if ended, found, err := p.OperationEnded(receipt.OperationID); err != nil || !found || ended {
		t.Fatal("an accepted operation that has not run has not ended", ended, found, err)
	}
	if _, found, err := p.OperationEnded("no-such-operation"); err != nil || found {
		t.Fatal("an unknown operation is not found", found, err)
	}
}
