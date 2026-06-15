package publicids

import (
	"errors"
	"testing"
)

// The exact deep link from the operator report, used as the anchoring golden
// case so the Go parser stays pinned to a real frontend URL.
const sampleStepURL = "https://glimmung.romaine.life/projects/ambience/issues/168/runs/9/cycles/1/phases/llm-verify/jobs/llm-verify/steps/run-verification"

func TestParseDashboardPathGolden(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want IssueRunAddress
	}{
		{
			name: "full step url",
			in:   sampleStepURL,
			want: IssueRunAddress{
				Kind: EntityStep, Project: "ambience", IssueNumber: 168,
				RunCycle: RunCycleAddress{Run: 9, Cycle: 1},
				Phase:    "llm-verify", Job: "llm-verify", Step: "run-verification",
			},
		},
		{
			name: "absolute path step",
			in:   "/projects/ambience/issues/168/runs/9/cycles/1/phases/llm-verify/jobs/llm-verify/steps/run-verification",
			want: IssueRunAddress{
				Kind: EntityStep, Project: "ambience", IssueNumber: 168,
				RunCycle: RunCycleAddress{Run: 9, Cycle: 1},
				Phase:    "llm-verify", Job: "llm-verify", Step: "run-verification",
			},
		},
		{
			name: "project",
			in:   "/projects/ambience",
			want: IssueRunAddress{Kind: EntityProject, Project: "ambience"},
		},
		{
			name: "issue",
			in:   "/projects/ambience/issues/168",
			want: IssueRunAddress{Kind: EntityIssue, Project: "ambience", IssueNumber: 168},
		},
		{
			name: "issue summary tab resolves to issue",
			in:   "/projects/ambience/issues/168/summary",
			want: IssueRunAddress{Kind: EntityIssue, Project: "ambience", IssueNumber: 168},
		},
		{
			name: "issue review tab resolves to issue",
			in:   "/projects/ambience/issues/168/review",
			want: IssueRunAddress{Kind: EntityIssue, Project: "ambience", IssueNumber: 168},
		},
		{
			name: "runs index",
			in:   "/projects/ambience/issues/168/runs",
			want: IssueRunAddress{Kind: EntityRuns, Project: "ambience", IssueNumber: 168},
		},
		{
			name: "run two-segment",
			in:   "/projects/ambience/issues/168/runs/9/cycles/1",
			want: IssueRunAddress{Kind: EntityRun, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 9, Cycle: 1}},
		},
		{
			name: "run dotted single segment",
			in:   "/projects/ambience/issues/168/runs/6.1",
			want: IssueRunAddress{Kind: EntityRun, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 6, Cycle: 1}},
		},
		{
			name: "phase",
			in:   "/projects/ambience/issues/168/runs/9/cycles/1/phases/llm-verify",
			want: IssueRunAddress{Kind: EntityPhase, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 9, Cycle: 1}, Phase: "llm-verify"},
		},
		{
			name: "job",
			in:   "/projects/ambience/issues/168/runs/9/cycles/1/phases/llm-verify/jobs/llm-verify",
			want: IssueRunAddress{Kind: EntityJob, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 9, Cycle: 1}, Phase: "llm-verify", Job: "llm-verify"},
		},
		{
			name: "trailing slash tolerated",
			in:   "/projects/ambience/issues/168/runs/9/cycles/1/",
			want: IssueRunAddress{Kind: EntityRun, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 9, Cycle: 1}},
		},
		{
			name: "query and fragment dropped",
			in:   sampleStepURL + "?foo=bar&x=1#section",
			want: IssueRunAddress{
				Kind: EntityStep, Project: "ambience", IssueNumber: 168,
				RunCycle: RunCycleAddress{Run: 9, Cycle: 1},
				Phase:    "llm-verify", Job: "llm-verify", Step: "run-verification",
			},
		},
		{
			name: "percent-encoded project decoded",
			in:   "/projects/foo%20bar/issues/3",
			want: IssueRunAddress{Kind: EntityIssue, Project: "foo bar", IssueNumber: 3},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseDashboardPath(tc.in)
			if err != nil {
				t.Fatalf("ParseDashboardPath(%q) error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseDashboardPath(%q)\n got = %+v\nwant = %+v", tc.in, got, tc.want)
			}
		})
	}
}

// TestParseDashboardPathRejects is the structural guard: anything that is not a
// recognized dashboard path must error rather than resolve to a wrong or
// partial address.
func TestParseDashboardPathRejects(t *testing.T) {
	for _, in := range []string{
		"",
		"   ",
		"projects/ambience", // no leading slash, no scheme
		"glimmung.romaine.life/projects/ambience/issues/1", // scheme-less host
		"/sessions/530",                                           // wrong root
		"/",                                                       // no segments
		"/projects",                                               // no project
		"/projects/ambience/widgets",                              // unknown after project
		"/projects/ambience/issues",                               // missing issue number
		"/projects/ambience/issues/abc",                           // non-numeric issue
		"/projects/ambience/issues/0",                             // non-positive issue
		"/projects/ambience/issues/-1",                            // negative issue
		"/projects/ambience/issues/168/bogustab",                  // unknown issue tab
		"/projects/ambience/issues/168/runs/9",                    // bare run, no cycle (the defect)
		"/projects/ambience/issues/168/runs/9/cycles/0",           // non-positive cycle
		"/projects/ambience/issues/168/runs/0/cycles/1",           // non-positive run
		"/projects/ambience/issues/168/runs/9/cycle/1",            // misspelled cycles
		"/projects/ambience/issues/168/runs/9.1.1",                // malformed dotted run
		"/projects/ambience/issues/168/runs/9/cycles/1/phases",    // phases without slug
		"/projects/ambience/issues/168/runs/9/cycles/1/widgets/x", // unknown sub-resource
		"/projects/ambience/issues/168/runs/9/cycles/1/phases/p/jobs/j/steps/s/more", // trailing garbage
	} {
		if got, err := ParseDashboardPath(in); err == nil {
			t.Fatalf("ParseDashboardPath(%q) = %+v; want error", in, got)
		}
	}
}

// TestParseDashboardPathDefectGuard is the targeted regression for the bug the
// publicids package was built to prevent: a bare issue-scoped run number in a
// dashboard URL must never resolve to a run cycle. It must error, and the error
// must be the run-cycle invariant, not a vague structural one.
func TestParseDashboardPathDefectGuard(t *testing.T) {
	_, err := ParseDashboardPath("/projects/ambience/issues/168/runs/9")
	if err == nil {
		t.Fatal("bare run number resolved; want error")
	}
	if !errors.Is(err, ErrInvalidRunCycleAddress) {
		t.Fatalf("error = %v; want ErrInvalidRunCycleAddress", err)
	}
}

func TestDashboardPathRoundTrip(t *testing.T) {
	addrs := []IssueRunAddress{
		{Kind: EntityProject, Project: "ambience"},
		{Kind: EntityIssue, Project: "ambience", IssueNumber: 168},
		{Kind: EntityRuns, Project: "ambience", IssueNumber: 168},
		{Kind: EntityRun, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 9, Cycle: 1}},
		{Kind: EntityPhase, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 9, Cycle: 1}, Phase: "llm-verify"},
		{Kind: EntityJob, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 9, Cycle: 1}, Phase: "llm-verify", Job: "llm-verify"},
		{Kind: EntityStep, Project: "ambience", IssueNumber: 168, RunCycle: RunCycleAddress{Run: 9, Cycle: 1}, Phase: "llm-verify", Job: "llm-verify", Step: "run-verification"},
		{Kind: EntityIssue, Project: "foo bar", IssueNumber: 3},
	}
	for _, want := range addrs {
		path, err := DashboardPath(want)
		if err != nil {
			t.Fatalf("DashboardPath(%+v) error: %v", want, err)
		}
		got, err := ParseDashboardPath(path)
		if err != nil {
			t.Fatalf("ParseDashboardPath(%q) error: %v", path, err)
		}
		if got != want {
			t.Fatalf("round trip via %q\n got = %+v\nwant = %+v", path, got, want)
		}
	}
}

// TestDashboardPathCanonical pins the rendered form to the two-segment
// runs/{run}/cycles/{cycle} spelling the frontend builder emits, including
// normalizing a parsed dotted run back to the canonical path.
func TestDashboardPathCanonical(t *testing.T) {
	step := IssueRunAddress{
		Kind: EntityStep, Project: "ambience", IssueNumber: 168,
		RunCycle: RunCycleAddress{Run: 9, Cycle: 1},
		Phase:    "llm-verify", Job: "llm-verify", Step: "run-verification",
	}
	const wantPath = "/projects/ambience/issues/168/runs/9/cycles/1/phases/llm-verify/jobs/llm-verify/steps/run-verification"
	got, err := DashboardPath(step)
	if err != nil {
		t.Fatalf("DashboardPath error: %v", err)
	}
	if got != wantPath {
		t.Fatalf("DashboardPath = %q, want %q", got, wantPath)
	}

	// A parsed dotted run renders back to the canonical two-segment form.
	addr, err := ParseDashboardPath("/projects/ambience/issues/168/runs/6.1")
	if err != nil {
		t.Fatalf("parse dotted run: %v", err)
	}
	rendered, err := DashboardPath(addr)
	if err != nil {
		t.Fatalf("render dotted run: %v", err)
	}
	if want := "/projects/ambience/issues/168/runs/6/cycles/1"; rendered != want {
		t.Fatalf("dotted run canonicalized to %q, want %q", rendered, want)
	}
}

// TestDashboardPathRejectsIncompleteAddress guards the renderer against
// addresses whose Kind claims more depth than their fields supply.
func TestDashboardPathRejectsIncompleteAddress(t *testing.T) {
	for _, addr := range []IssueRunAddress{
		{Kind: EntityProject},                           // no project
		{Kind: EntityIssue, Project: "p"},               // no issue number
		{Kind: EntityRun, Project: "p", IssueNumber: 1}, // no run cycle
		{Kind: EntityPhase, Project: "p", IssueNumber: 1, RunCycle: RunCycleAddress{1, 1}},                      // no phase
		{Kind: EntityStep, Project: "p", IssueNumber: 1, RunCycle: RunCycleAddress{1, 1}, Phase: "x", Job: "y"}, // no step
	} {
		if got, err := DashboardPath(addr); err == nil {
			t.Fatalf("DashboardPath(%+v) = %q; want error", addr, got)
		}
	}
}
