package publicids

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ErrInvalidDashboardPath is returned when a string is not a parseable
// glimmung dashboard URL or absolute path.
var ErrInvalidDashboardPath = errors.New("invalid dashboard path")

// EntityKind names the deepest level a dashboard address resolves to. It is the
// discriminator for IssueRunAddress: fields below the named level are zero.
type EntityKind string

const (
	EntityProject EntityKind = "project"
	EntityIssue   EntityKind = "issue"
	EntityRuns    EntityKind = "runs"
	EntityRun     EntityKind = "run"
	EntityPhase   EntityKind = "phase"
	EntityJob     EntityKind = "job"
	EntityStep    EntityKind = "step"
)

// IssueRunAddress is the structured form of a glimmung dashboard deep link such
// as
//
//	/projects/ambience/issues/168/runs/9/cycles/1/phases/llm-verify/jobs/llm-verify/steps/run-verification
//
// It is the canonical inverse of the frontend issueRunSelectionPath builder and
// the route table in frontend/src/routes.ts. Kind names how deep the address
// goes; callers read fields only up to that depth (a EntityIssue address has a
// zero RunCycle/Phase/Job/Step, and so on).
type IssueRunAddress struct {
	Kind        EntityKind
	Project     string          // every kind
	IssueNumber int             // EntityIssue and deeper
	RunCycle    RunCycleAddress // EntityRun and deeper
	Phase       string          // EntityPhase and deeper
	Job         string          // EntityJob and deeper
	Step        string          // EntityStep
}

// issueChildViews are the issue-scoped SPA tabs that resolve to the issue
// resource itself (no deeper addressing). Kept in sync with
// ISSUE_DETAIL_CHILD_ROUTES in frontend/src/routes.ts; "runs" is handled
// separately because it descends into the run hierarchy.
var issueChildViews = map[string]bool{
	"summary":    true,
	"settings":   true,
	"touchpoint": true,
}

// ParseDashboardPath parses a glimmung dashboard URL or absolute path into a
// structured address. It accepts a full URL ("https://glimmung.romaine.life/
// projects/...") or an absolute path ("/projects/..."); query strings and
// fragments are ignored and percent-encoded segments are decoded.
//
// It is strict by construction: it is the inverse of the frontend route table,
// rejects anything that is not a recognized dashboard path, and — mirroring
// ParseRunCycleAddress — refuses to resolve an ambiguous bare run number so a
// deep link can never silently address the wrong run cycle.
func ParseDashboardPath(raw string) (IssueRunAddress, error) {
	segs, err := dashboardPathSegments(raw)
	if err != nil {
		return IssueRunAddress{}, err
	}

	if len(segs) < 2 || segs[0] != "projects" {
		return IssueRunAddress{}, fmt.Errorf("%w: expected /projects/{project}, got %q", ErrInvalidDashboardPath, raw)
	}
	project := segs[1]
	if project == "" {
		return IssueRunAddress{}, fmt.Errorf("%w: empty project segment", ErrInvalidDashboardPath)
	}
	addr := IssueRunAddress{Kind: EntityProject, Project: project}

	rest := segs[2:]
	if len(rest) == 0 {
		return addr, nil
	}

	if rest[0] != "issues" {
		return IssueRunAddress{}, fmt.Errorf("%w: expected issues segment, got %q", ErrInvalidDashboardPath, rest[0])
	}
	if len(rest) < 2 {
		return IssueRunAddress{}, fmt.Errorf("%w: missing issue number", ErrInvalidDashboardPath)
	}
	issueNumber, ok := positiveInt(rest[1])
	if !ok {
		return IssueRunAddress{}, fmt.Errorf("%w: issue number %q must be a positive integer", ErrInvalidDashboardPath, rest[1])
	}
	addr.Kind = EntityIssue
	addr.IssueNumber = issueNumber

	rest = rest[2:]
	if len(rest) == 0 {
		return addr, nil
	}

	if rest[0] != "runs" {
		// Issue-scoped views that are not the run hierarchy resolve to the
		// issue resource itself.
		if len(rest) == 1 && issueChildViews[rest[0]] {
			return addr, nil
		}
		return IssueRunAddress{}, fmt.Errorf("%w: unknown issue view %q", ErrInvalidDashboardPath, strings.Join(rest, "/"))
	}

	rest = rest[1:]
	if len(rest) == 0 {
		addr.Kind = EntityRuns
		return addr, nil
	}

	runCycle, consumed, err := parseRunCycleSegmentsTail(rest)
	if err != nil {
		return IssueRunAddress{}, err
	}
	addr.Kind = EntityRun
	addr.RunCycle = runCycle

	rest = rest[consumed:]
	if len(rest) == 0 {
		return addr, nil
	}

	// phases/{phase}[/jobs/{job}[/steps/{step}]]
	if rest[0] != "phases" || len(rest) < 2 {
		return IssueRunAddress{}, fmt.Errorf("%w: expected phases/{phase}, got %q", ErrInvalidDashboardPath, strings.Join(rest, "/"))
	}
	if rest[1] == "" {
		return IssueRunAddress{}, fmt.Errorf("%w: empty phase segment", ErrInvalidDashboardPath)
	}
	addr.Kind = EntityPhase
	addr.Phase = rest[1]

	rest = rest[2:]
	if len(rest) == 0 {
		return addr, nil
	}

	if rest[0] != "jobs" || len(rest) < 2 {
		return IssueRunAddress{}, fmt.Errorf("%w: expected jobs/{job}, got %q", ErrInvalidDashboardPath, strings.Join(rest, "/"))
	}
	if rest[1] == "" {
		return IssueRunAddress{}, fmt.Errorf("%w: empty job segment", ErrInvalidDashboardPath)
	}
	addr.Kind = EntityJob
	addr.Job = rest[1]

	rest = rest[2:]
	if len(rest) == 0 {
		return addr, nil
	}

	if rest[0] != "steps" || len(rest) < 2 {
		return IssueRunAddress{}, fmt.Errorf("%w: expected steps/{step}, got %q", ErrInvalidDashboardPath, strings.Join(rest, "/"))
	}
	if rest[1] == "" {
		return IssueRunAddress{}, fmt.Errorf("%w: empty step segment", ErrInvalidDashboardPath)
	}
	addr.Kind = EntityStep
	addr.Step = rest[1]

	rest = rest[2:]
	if len(rest) != 0 {
		return IssueRunAddress{}, fmt.Errorf("%w: unexpected trailing segments %q", ErrInvalidDashboardPath, strings.Join(rest, "/"))
	}
	return addr, nil
}

// parseRunCycleSegmentsTail reads the run-cycle address from the segments that
// follow "runs", returning how many segments it consumed. It accepts the
// two-segment form the frontend builder emits (runs/{run}/cycles/{cycle}) and
// the single dotted form (runs/{run.cycle}). A bare run number with no cycle is
// rejected by the underlying parsers.
func parseRunCycleSegmentsTail(segs []string) (RunCycleAddress, int, error) {
	if len(segs) >= 3 && segs[1] == "cycles" {
		addr, err := ParseRunCycleSegments(segs[0], segs[2])
		if err != nil {
			return RunCycleAddress{}, 0, err
		}
		return addr, 3, nil
	}
	addr, err := ParseRunCycleAddress(segs[0])
	if err != nil {
		return RunCycleAddress{}, 0, err
	}
	return addr, 1, nil
}

// DashboardPath renders the canonical absolute dashboard path for an address.
// It is the inverse of ParseDashboardPath and emits the two-segment
// runs/{run}/cycles/{cycle} form the frontend issueRunSelectionPath builder
// produces. The result is scheme/host-less; callers that need an absolute URL
// prepend the external base.
func DashboardPath(addr IssueRunAddress) (string, error) {
	if addr.Project == "" {
		return "", fmt.Errorf("%w: address has no project", ErrInvalidDashboardPath)
	}
	var b strings.Builder
	b.WriteString("/projects/")
	b.WriteString(url.PathEscape(addr.Project))
	if addr.Kind == EntityProject {
		return b.String(), nil
	}

	if addr.IssueNumber < 1 {
		return "", fmt.Errorf("%w: %s address needs a positive issue number", ErrInvalidDashboardPath, addr.Kind)
	}
	b.WriteString("/issues/")
	b.WriteString(strconv.Itoa(addr.IssueNumber))
	switch addr.Kind {
	case EntityIssue:
		return b.String(), nil
	case EntityRuns:
		b.WriteString("/runs")
		return b.String(), nil
	}

	if addr.RunCycle.Run < 1 || addr.RunCycle.Cycle < 1 {
		return "", fmt.Errorf("%w: %s address needs a valid run cycle", ErrInvalidDashboardPath, addr.Kind)
	}
	b.WriteString("/runs/")
	b.WriteString(strconv.Itoa(addr.RunCycle.Run))
	b.WriteString("/cycles/")
	b.WriteString(strconv.Itoa(addr.RunCycle.Cycle))
	if addr.Kind == EntityRun {
		return b.String(), nil
	}

	if addr.Phase == "" {
		return "", fmt.Errorf("%w: %s address needs a phase", ErrInvalidDashboardPath, addr.Kind)
	}
	b.WriteString("/phases/")
	b.WriteString(url.PathEscape(addr.Phase))
	if addr.Kind == EntityPhase {
		return b.String(), nil
	}

	if addr.Job == "" {
		return "", fmt.Errorf("%w: %s address needs a job", ErrInvalidDashboardPath, addr.Kind)
	}
	b.WriteString("/jobs/")
	b.WriteString(url.PathEscape(addr.Job))
	if addr.Kind == EntityJob {
		return b.String(), nil
	}

	if addr.Step == "" {
		return "", fmt.Errorf("%w: step address needs a step", ErrInvalidDashboardPath)
	}
	b.WriteString("/steps/")
	b.WriteString(url.PathEscape(addr.Step))
	if addr.Kind == EntityStep {
		return b.String(), nil
	}

	return "", fmt.Errorf("%w: unknown entity kind %q", ErrInvalidDashboardPath, addr.Kind)
}

// dashboardPathSegments extracts decoded, non-empty path segments from a full
// URL or absolute path. Query strings and fragments are dropped.
func dashboardPathSegments(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: empty input", ErrInvalidDashboardPath)
	}

	var p string
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidDashboardPath, err)
		}
		p = u.Path
	case strings.HasPrefix(raw, "/"):
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			raw = raw[:i]
		}
		p = raw
	default:
		return nil, fmt.Errorf("%w: expected an absolute path or full URL, got %q", ErrInvalidDashboardPath, raw)
	}

	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		dec, err := url.PathUnescape(part)
		if err != nil {
			return nil, fmt.Errorf("%w: undecodable segment %q: %v", ErrInvalidDashboardPath, part, err)
		}
		out = append(out, dec)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no path segments", ErrInvalidDashboardPath)
	}
	return out, nil
}
