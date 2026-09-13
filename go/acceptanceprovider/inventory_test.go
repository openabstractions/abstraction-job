package acceptanceprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	identity "github.com/openabstractions/abstraction-identity"
	"github.com/openabstractions/abstraction-identity/listen"
	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInventoryTraversesMixedScopesAndReplays(t *testing.T) {
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	expected := map[string]bool{}
	for i := 0; i < 280; i++ {
		scope := "other"
		if i%5 == 0 {
			scope = "own"
			expected[fmt.Sprint(i)] = true
		}
		accept(t, p.Bind(scope), submission(p, fmt.Sprint(i)))
	}
	b := p.BindInventory("own")
	cursor := ""
	seen := map[string]bool{}
	var old string
	for pages := 0; pages < 100; pages++ {
		input := cursor
		page, err := b.ListWork(input, 7)
		if err != nil || page.Outcome != "page" {
			t.Fatalf("page: %+v %v", page, err)
		}
		if input != "" {
			replay, err := b.ListWork(input, 7)
			if err != nil || !reflect.DeepEqual(page, replay) {
				t.Fatal("lost-reply replay changed")
			}
			foreign, _ := p.BindInventory("other").ListWork(input, 7)
			if foreign.Outcome != "gap" {
				t.Fatal("cursor crossed scope")
			}
			if old != "" {
				stale, _ := b.ListWork(old, 7)
				if stale.Outcome != "gap" {
					t.Fatal("old cursor did not gap")
				}
			}
			old = input
		}
		for _, s := range page.Snapshots {
			key := s.Receipt.Identity.Key
			if !expected[key] || seen[key] {
				t.Fatal("wrong scope or repeated identity", key)
			}
			seen[key] = true
		}
		if page.Complete {
			if page.Next != "" {
				t.Fatal("complete cursor")
			}
			break
		}
		if page.Next == "" || page.Next == input {
			t.Fatal("continuation did not advance")
		}
		cursor = page.Next
	}
	if !reflect.DeepEqual(expected, seen) {
		t.Fatalf("traversal lost entries: %d/%d", len(seen), len(expected))
	}
}
func TestInventoryEmptyPagesContinueAndLifetime(t *testing.T) {
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	for i := 0; i < 270; i++ {
		accept(t, p.Bind("other"), submission(p, fmt.Sprint(i)))
	}
	b := p.BindInventory("own")
	page, _ := b.ListWork("", 64)
	if page.Outcome != "page" || page.Complete || len(page.Snapshots) != 0 || page.Next == "" {
		t.Fatalf("empty scan page: %+v", page)
	}
	end := page
	for pages := 0; pages < 8 && !end.Complete; pages++ {
		end, _ = b.ListWork(end.Next, 64)
	}
	if !end.Complete || len(end.Snapshots) != 0 {
		t.Fatalf("empty continuation: %+v", end)
	}
	token := strings.Split(page.Next, ":")[0]
	p.inventory.mu.Lock()
	session := p.inventory.sessions[token]
	session.expires = time.Now().Add(-time.Second)
	p.inventory.mu.Unlock()
	p.expireInventory(token, session)
	gap, _ := b.ListWork(page.Next, 64)
	if gap.Outcome != "gap" {
		t.Fatal("expiry hidden")
	}
	fresh, _ := b.ListWork("", 1)
	token = strings.Split(fresh.Next, ":")[0]
	p.inventory.mu.Lock()
	session = p.inventory.sessions[token]
	handle := session.dir
	p.inventory.mu.Unlock()
	if err := p.CloseInventory(); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Stat(); err == nil {
		t.Fatal("directory handle retained")
	}
	if len(p.inventory.sessions) != 0 {
		t.Fatal("sessions retained")
	}
	unavailable, _ := b.ListWork("", 1)
	if unavailable.Outcome != "unavailable" {
		t.Fatal("closed inventory admitted")
	}
}
func TestInventoryBoundsAndRefusal(t *testing.T) {
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	accept(t, p.Bind("own"), submission(p, "one"))
	accept(t, p.Bind("own"), submission(p, "two"))
	b := p.BindInventory("own")
	for _, limit := range []int64{0, 65} {
		v, _ := b.ListWork("", limit)
		if v.Outcome != "invalid" {
			t.Fatal(v)
		}
	}
	forbidden, _ := p.BindInventory("").ListWork("", 1)
	if forbidden.Outcome != "forbidden" || len(p.inventory.sessions) != 0 {
		t.Fatal("unauthorized inventory read")
	}
	var cursor string
	for i := 0; i < inventorySessions; i++ {
		v, _ := b.ListWork("", 1)
		if v.Outcome != "page" {
			t.Fatal(v)
		}
		cursor = v.Next
	}
	full, _ := b.ListWork("", 1)
	if full.Outcome != "unavailable" {
		t.Fatal("unbounded sessions")
	}
	p2 := openTest(t, p.root)
	defer p2.CloseInventory()
	gap, _ := p2.BindInventory("own").ListWork(cursor, 1)
	if gap.Outcome != "gap" {
		t.Fatal("restart accepted cursor")
	}
}
func TestInventoryReplaysIndependentValues(t *testing.T) {
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	for i := 0; i < 3; i++ {
		accept(t, p.Bind("own"), submission(p, fmt.Sprint(i)))
	}
	b := p.BindInventory("own")
	first, _ := b.ListWork("", 1)
	second, _ := b.ListWork(first.Next, 1)
	original, _ := json.Marshal(second)
	second.Snapshots[0].Receipt.AcceptedGuarantees[0] = "modified-by-caller"
	replay, _ := b.ListWork(first.Next, 1)
	again, _ := json.Marshal(replay)
	if string(original) != string(again) {
		t.Fatal("caller mutated replay")
	}
}
func TestInventoryByteBudgetAndCorruptFiles(t *testing.T) {
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	accept(t, p.Bind("own"), submission(p, "one"))
	// Oversized private files must refuse, never allocate beyond the read cap.
	path := p.requestPath("own", submission(p, "one").Identity)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(8 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	v, _ := p.BindInventory("own").ListWork("", 1)
	if v.Outcome != "unavailable" || len(v.Snapshots) != 0 || v.Complete || v.Next != "" {
		t.Fatalf("oversize: %+v", v)
	}
}

var _ api.JobInventory = (*bound)(nil)

func TestInventoryPolicyRecheckedBeforeReplay(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("current Program peer proof unavailable")
	}
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	for i := 0; i < 3; i++ {
		accept(t, p.Bind("caller"), submission(p, fmt.Sprint(i)))
	}
	var first, second api.InventoryPage
	allowed := func(context.Context, *identity.Peer, string, string) error { return nil }
	methodExchange(t, p, allowed, func(tr listen.FrameClient) {
		var err error
		first, err = api.NewJobInventoryClient(tr).ListWork("", 1)
		if err != nil {
			t.Fatal(err)
		}
	})
	methodExchange(t, p, allowed, func(tr listen.FrameClient) {
		var err error
		second, err = api.NewJobInventoryClient(tr).ListWork(first.Next, 1)
		if err != nil {
			t.Fatal(err)
		}
	})
	if second.Outcome != "page" {
		t.Fatal(second)
	}
	denied := func(context.Context, *identity.Peer, string, string) error {
		return errors.New("private policy reason")
	}
	methodExchange(t, p, denied, func(tr listen.FrameClient) {
		page, err := api.NewJobInventoryClient(tr).ListWork(first.Next, 1)
		if err != nil || page.Outcome != "forbidden" || len(page.Snapshots) != 0 {
			t.Fatalf("cached authorization bypass: %+v %v", page, err)
		}
	})
}

type inventoryFailureExecutor struct{ testExecutor }

func (inventoryFailureExecutor) OperationFailure(*job.Record) *api.WorkFailure {
	return &api.WorkFailure{Classification: "unknown", Message: strings.Repeat("sensitive provider diagnostic", 100000)}
}
func TestInventoryLargeFailureIsBounded(t *testing.T) {
	p, err := OpenWithExecutor(t.TempDir(), "owner", inventoryFailureExecutor{testExecutor{profile: "inventory-test"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.CloseInventory()
	accept(t, p.Bind("own"), submission(p, "one"))
	page, _ := p.BindInventory("own").ListWork("", 1)
	encoded, _ := json.Marshal(page)
	if page.Outcome != "page" || len(encoded) > inventoryPageBytes || strings.Contains(string(encoded), "sensitive") {
		t.Fatalf("unbounded failure: %d", len(encoded))
	}
}
func TestInventoryReadBudgetContinues(t *testing.T) {
	p := openTest(t, t.TempDir())
	defer p.CloseInventory()
	for i := 0; i < 8; i++ {
		s := submission(p, fmt.Sprint(i))
		s.Spec = []byte(`{"large":"` + strings.Repeat("x", 900000) + `"}`)
		accept(t, p.Bind("own"), s)
	}
	b := p.BindInventory("own")
	page, _ := b.ListWork("", 64)
	if page.Outcome != "page" || page.Complete || len(page.Snapshots) == 0 || len(page.Snapshots) >= 8 {
		t.Fatalf("read budget not exercised: %+v", page)
	}
	count := len(page.Snapshots)
	for i := 0; i < 10 && !page.Complete; i++ {
		page, _ = b.ListWork(page.Next, 64)
		if page.Outcome != "page" {
			t.Fatal(page)
		}
		count += len(page.Snapshots)
	}
	if !page.Complete || count != 8 {
		t.Fatalf("byte-budget continuation lost entries: %d", count)
	}
}
