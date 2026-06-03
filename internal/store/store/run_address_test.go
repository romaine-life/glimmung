package store

import (
	"testing"

	"github.com/romaine-life/glimmung/internal/domain/publicids"
)

// ambienceIssue168Docs mirrors the durable shape that produced the reported
// bug. On this issue, run 6's first cycle is the 9th cycle overall
// (cycle_number=9, run_display_number="6.1"), and run 4's first cycle is the
// 6th (cycle_number=6, run_display_number="4.1"). The cycle ledger (9, 6) and
// the run number (6, 4) therefore collide across runs — which is exactly why
// addressing by the ledger returned the wrong run.
func ambienceIssue168Docs() []runDoc {
	return []runDoc{
		{
			ID: "r4c1", Project: "ambience", IssueNumber: 168,
			RunNumber: intPtr(4), RunCycleNumber: intPtr(1), CycleNumber: intPtr(6),
			RunDisplayNumber: stringPtr("4.1"),
		},
		{
			ID: "r6c1", Project: "ambience", IssueNumber: 168,
			RunNumber: intPtr(6), RunCycleNumber: intPtr(1), CycleNumber: intPtr(9),
			RunDisplayNumber: stringPtr("6.1"),
		},
	}
}

func TestMatchRunDocByAddressResolvesCanonicalRunCycle(t *testing.T) {
	docs := ambienceIssue168Docs()
	addr, err := publicids.ParseRunCycleAddress("6.1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	doc, ok := matchRunDocByAddress(docs, addr)
	if !ok || doc.ID != "r6c1" {
		t.Fatalf("address 6.1 resolved to ok=%v id=%q, want r6c1", ok, doc.ID)
	}
}

// TestMatchRunDocByAddressIgnoresCycleLedger is the store-level migration guard.
// Address "9.1" (run 9, cycle 1) must NOT resolve to the run whose CycleNumber
// is 9 (the "6.1" run). If this test ever finds a match, the deleted
// ledger-matching overload has been reintroduced.
func TestMatchRunDocByAddressIgnoresCycleLedger(t *testing.T) {
	docs := ambienceIssue168Docs()
	for _, absent := range []string{"9.1", "6.2", "4.2", "6.9", "99.1"} {
		addr, err := publicids.ParseRunCycleAddress(absent)
		if err != nil {
			t.Fatalf("parse %q: %v", absent, err)
		}
		if doc, ok := matchRunDocByAddress(docs, addr); ok {
			t.Fatalf("address %q must not resolve; matched %q (cycle_number=%v)", absent, doc.ID, deref(doc.CycleNumber))
		}
	}
}

// TestMatchRunDocByAddressMatchesViaDisplayWhenIntsAbsent verifies the
// canonical match still works for docs that carry only run_display_number
// (e.g. partially-migrated rows) without falling back to the ledger.
func TestMatchRunDocByAddressMatchesViaDisplayWhenIntsAbsent(t *testing.T) {
	docs := []runDoc{{
		ID: "display-only", Project: "ambience", IssueNumber: 168,
		CycleNumber: intPtr(9), RunDisplayNumber: stringPtr("6.1"),
	}}
	addr, _ := publicids.ParseRunCycleAddress("6.1")
	if doc, ok := matchRunDocByAddress(docs, addr); !ok || doc.ID != "display-only" {
		t.Fatalf("display-only match: ok=%v id=%q", ok, doc.ID)
	}
	// The ledger value 9 is present but must not be addressable as run 9 cycle 1.
	ledger, _ := publicids.ParseRunCycleAddress("9.1")
	if _, ok := matchRunDocByAddress(docs, ledger); ok {
		t.Fatalf("ledger 9.1 must not resolve the display-only doc")
	}
}

func deref(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}
