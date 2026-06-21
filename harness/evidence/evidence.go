// Package evidence holds generic, deterministic evidence matchers and a TRX
// (Visual Studio test results) parser ported from spirelens'
// .github/scripts/lib/EvidenceContract.ps1 and UnitTestResult.ps1. These are
// the boundary-exact comparisons that keep a verifier honest: a claimed
// "vigor gained: 8" must match "vigor gained: 8" but never "vigor gained: 88",
// and an id token "CLOAK_CLASP" must never match "CLOAK_CLASP_PLUS".
//
// Go's regexp engine (RE2) has no lookaround, so the boundary checks are done
// by an explicit case-insensitive scan rather than the PowerShell regex; the
// behavior is identical and the ported Pester cases prove it.
package evidence

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// ExpectedTextMatch reports whether observed contains expected at a token
// boundary, case-insensitively. A boundary is the string start/end or any
// non-alphanumeric character, so "vigor gained: 8" matches inside
// "Akabeko vigor gained: 8" but NOT "vigor gained: 88" / ": 80" / ": 800".
//
// An empty or whitespace-only expected is a no-op that returns true: presence
// of the assertion is enforced upstream, not here. This mirrors
// Test-ExpectedTextMatch.
func ExpectedTextMatch(expected, observed string) bool {
	needle := strings.TrimSpace(expected)
	if needle == "" {
		return true
	}
	return tokenPresent(observed, needle, isAlnum)
}

// GameStateJSONContainsID reports whether the canonical id token appears in
// jsonStr at an id boundary (not preceded or followed by [A-Za-z0-9_]),
// case-insensitively. "CLOAK_CLASP" matches `{"id":"cloak_clasp"}` but NOT
// `{"id":"CLOAK_CLASP_PLUS"}`. Empty json or id returns false. This mirrors
// Test-GameStateJsonContainsId.
func GameStateJSONContainsID(jsonStr, id string) bool {
	if strings.TrimSpace(jsonStr) == "" || strings.TrimSpace(id) == "" {
		return false
	}
	return tokenPresent(jsonStr, strings.TrimSpace(id), isAlnumUnderscore)
}

// tokenPresent scans haystack (case-insensitively) for needle and returns true
// when at least one occurrence has non-boundary neighbours on both sides,
// where isBoundaryChar reports whether a byte continues a token. Literal
// substring search means regex-special characters in needle are matched
// verbatim (stronger than [regex]::Escape).
func tokenPresent(haystack, needle string, isBoundaryChar func(byte) bool) bool {
	h := strings.ToLower(haystack)
	n := strings.ToLower(needle)
	if n == "" {
		return false
	}
	from := 0
	for from+len(n) <= len(h) {
		idx := strings.Index(h[from:], n)
		if idx < 0 {
			return false
		}
		start := from + idx
		end := start + len(n)
		beforeOK := start == 0 || !isBoundaryChar(h[start-1])
		afterOK := end == len(h) || !isBoundaryChar(h[end])
		if beforeOK && afterOK {
			return true
		}
		from = start + 1
	}
	return false
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isAlnumUnderscore(b byte) bool {
	return isAlnum(b) || b == '_'
}

// UnitTestResult is the structured verdict of a dotnet test run, mirroring the
// pscustomobject Get-ObservedUnitTestResult returns.
type UnitTestResult struct {
	Passed      bool
	Total       int
	Failed      int
	FailedNames []string
	Notes       string
}

// ObservedUnitTestResult reads a TRX file and combines it with the process
// exit code into a structured verdict. The verdict is exitCode == 0 AND
// failed == 0 — a nonzero exit can never be laundered into a pass by a clean
// counter, and a missing/unparseable TRX falls back to the exit code with a
// synthetic note. This mirrors Get-ObservedUnitTestResult.
func ObservedUnitTestResult(exitCode int, trxPath string) UnitTestResult {
	if strings.TrimSpace(trxPath) == "" {
		return synthetic(exitCode, fmt.Sprintf("Unit-test TRX was not found at '%s'.", trxPath))
	}
	data, err := os.ReadFile(trxPath)
	if err != nil {
		return synthetic(exitCode, fmt.Sprintf("Unit-test TRX was not found at '%s'.", trxPath))
	}
	total, failed, failedNames, perr := ParseTRX(data)
	if perr != nil {
		return synthetic(exitCode, fmt.Sprintf("Unit-test TRX at '%s' could not be parsed: %v.", trxPath, perr))
	}

	passed := exitCode == 0 && failed == 0
	var note string
	switch {
	case passed:
		note = fmt.Sprintf("%d unit test(s) passed (exit %d, 0 failed).", total, exitCode)
	case len(failedNames) > 0:
		note = fmt.Sprintf("%d of %d unit test(s) failed (exit %d). Failing: %s.", failed, total, exitCode, strings.Join(failedNames, "; "))
	default:
		note = fmt.Sprintf("dotnet test reported failure (exit %d) for %d unit test(s); the TRX enumerated %d failed but no failing test names.", exitCode, total, failed)
	}
	return UnitTestResult{
		Passed:      passed,
		Total:       total,
		Failed:      failed,
		FailedNames: failedNames,
		Notes:       note,
	}
}

func synthetic(exitCode int, parseNote string) UnitTestResult {
	if exitCode == 0 {
		return UnitTestResult{
			Passed:      true,
			FailedNames: []string{},
			Notes:       strings.TrimSpace("dotnet test reported success (exit 0) but no structured TRX result was available. " + parseNote),
		}
	}
	return UnitTestResult{
		Passed:      false,
		FailedNames: []string{},
		Notes:       strings.TrimSpace(fmt.Sprintf("dotnet test failed (exit %d) and no structured TRX result was available to enumerate failing tests. %s", exitCode, parseNote)),
	}
}

// ParseTRX parses a VSTest TRX document, returning the total and failed counts
// from ResultSummary/Counters and the names of UnitTestResult rows whose
// outcome is "Failed". It is namespace-agnostic (matches on local element
// names) because the VSTest schema lives in a namespace the consumer should
// not have to bind. Enumerated failing rows trump a lower/absent summary
// counter, defending against an aborted runner that wrote result rows but an
// incomplete summary.
func ParseTRX(data []byte) (total, failed int, failedNames []string, err error) {
	failedNames = []string{}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	sawAny := false
	for {
		tok, terr := dec.Token()
		if terr != nil {
			if errors.Is(terr, io.EOF) {
				break
			}
			return 0, 0, nil, terr
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		sawAny = true
		switch start.Name.Local {
		case "Counters":
			for _, attr := range start.Attr {
				switch attr.Name.Local {
				case "total":
					total = atoiSafe(attr.Value, total)
				case "failed":
					failed = atoiSafe(attr.Value, failed)
				}
			}
		case "UnitTestResult":
			var outcome, name string
			for _, attr := range start.Attr {
				switch attr.Name.Local {
				case "outcome":
					outcome = attr.Value
				case "testName":
					name = attr.Value
				}
			}
			if outcome == "Failed" {
				if strings.TrimSpace(name) == "" {
					name = "(unnamed test)"
				}
				failedNames = append(failedNames, name)
			}
		}
	}
	if !sawAny {
		return 0, 0, nil, fmt.Errorf("no XML elements found")
	}
	if len(failedNames) > failed {
		failed = len(failedNames)
	}
	return total, failed, failedNames, nil
}

func atoiSafe(value string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
		return n
	}
	return fallback
}
