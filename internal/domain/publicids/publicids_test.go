package publicids

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type goldenCase struct {
	Name        string `json:"name"`
	Function    string `json:"function"`
	Project     string `json:"project"`
	Repo        string `json:"repo"`
	Number      *int   `json:"number"`
	IssueNumber *int   `json:"issue_number"`
	RunDisplay  string `json:"run_display"`
	SlotName    string `json:"slot_name"`
	LeaseNumber *int   `json:"lease_number"`
	Want        string `json:"want"`
}

func TestPublicIDGoldenParity(t *testing.T) {
	for _, tc := range loadGoldenCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			got := runPublicIDCase(t, tc)
			if got != tc.Want {
				t.Fatalf("got %q, want %q", got, tc.Want)
			}
		})
	}
}

func runPublicIDCase(t *testing.T, tc goldenCase) string {
	t.Helper()

	switch tc.Function {
	case "issue_ref":
		return IssueRef(tc.Project, tc.Number)
	case "run_ref":
		if tc.IssueNumber == nil {
			t.Fatalf("run_ref golden case %q must define issue_number", tc.Name)
		}
		return RunRef(tc.Project, *tc.IssueNumber, tc.RunDisplay)
	case "touchpoint_ref":
		return TouchpointRef(tc.Repo, tc.Number)
	case "lease_ref":
		return LeaseRef(tc.Project, tc.SlotName, tc.LeaseNumber)
	default:
		t.Fatalf("unknown public ID function %q", tc.Function)
		return ""
	}
}

func TestParseRunCycleAddressAcceptsCanonical(t *testing.T) {
	cases := map[string]RunCycleAddress{
		"6.1":   {Run: 6, Cycle: 1},
		"1.1":   {Run: 1, Cycle: 1},
		"12.34": {Run: 12, Cycle: 34},
		" 6.1 ": {Run: 6, Cycle: 1},
	}
	for in, want := range cases {
		got, err := ParseRunCycleAddress(in)
		if err != nil {
			t.Fatalf("ParseRunCycleAddress(%q) error: %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseRunCycleAddress(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// TestParseRunCycleAddressRejectsNonCanonical is the parser-level guard behind
// the reported bug: a bare run number, a flat cycle-ledger integer, or any
// malformed token must not parse, so it can never be matched against a run.
func TestParseRunCycleAddressRejectsNonCanonical(t *testing.T) {
	for _, in := range []string{"", "9", "6", "0.1", "6.0", "-1.1", "6.-1", "6.1.1", "a.b", "6.", ".1", "6.x", "ledger", "6 1", "6,1"} {
		if got, err := ParseRunCycleAddress(in); err == nil {
			t.Fatalf("ParseRunCycleAddress(%q) = %+v; want error", in, got)
		}
	}
}

func TestRunCycleAddressStringRoundTrips(t *testing.T) {
	for _, s := range []string{"6.1", "1.1", "12.34"} {
		addr, err := ParseRunCycleAddress(s)
		if err != nil {
			t.Fatalf("ParseRunCycleAddress(%q) error: %v", s, err)
		}
		if addr.String() != s {
			t.Fatalf("round trip %q -> %q", s, addr.String())
		}
	}
}

func TestParseRunCycleSegments(t *testing.T) {
	got, err := ParseRunCycleSegments("6", "1")
	if err != nil || got != (RunCycleAddress{Run: 6, Cycle: 1}) {
		t.Fatalf("ParseRunCycleSegments(6,1) = %+v, %v", got, err)
	}
	// A dotted run segment belongs to the single-segment routes, not here.
	if _, err := ParseRunCycleSegments("6.1", "1"); err == nil {
		t.Fatalf("dotted run segment must be rejected")
	}
	for _, seg := range [][2]string{{"0", "1"}, {"6", "0"}, {"", "1"}, {"6", ""}, {"a", "1"}, {"6", "b"}} {
		if _, err := ParseRunCycleSegments(seg[0], seg[1]); err == nil {
			t.Fatalf("ParseRunCycleSegments(%q,%q) must be rejected", seg[0], seg[1])
		}
	}
}

func loadGoldenCases(t *testing.T) []goldenCase {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "..", "testdata", "public_id_cases.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden cases: %v", err)
	}

	var cases []goldenCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatalf("decode golden cases: %v", err)
	}
	return cases
}
