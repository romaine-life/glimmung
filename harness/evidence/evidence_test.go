package evidence

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These cases are ported verbatim from spirelens'
// .github/scripts/tests/EvidenceGuard.Tests.ps1 and UnitTestResult.Tests.ps1
// so the boundary-exact contract is preserved across the shell->Go migration.

func TestExpectedTextMatch_NumericExactness(t *testing.T) {
	// passes at a boundary, case-insensitive.
	mustMatch(t, "vigor gained: 8", "Akabeko vigor gained: 8")
	mustMatch(t, "Vigor Gained: 8", "akabeko vigor gained: 8")

	// 8 must not match 88 / 80 / 800.
	mustNotMatch(t, "vigor gained: 8", "Akabeko vigor gained: 88")
	mustNotMatch(t, "vigor gained: 8", "vigor gained: 80")
	mustNotMatch(t, "vigor gained: 8", "vigor gained: 800")

	// absent / empty observed.
	mustNotMatch(t, "vigor gained: 8", "Akabeko tooltip rendered")
	mustNotMatch(t, "vigor gained: 8", "")

	// start / middle / end / whole-string positions.
	mustMatch(t, "vigor gained: 8", "vigor gained: 8")
	mustMatch(t, "vigor gained: 8", "vigor gained: 8 in tooltip")
	mustMatch(t, "vigor gained: 8", "tooltip says vigor gained: 8")

	// empty needle is a no-op (presence enforced upstream).
	if !ExpectedTextMatch("", "whatever") {
		t.Fatal("empty needle should match")
	}
	if !ExpectedTextMatch("   ", "whatever") {
		t.Fatal("whitespace needle should match")
	}
}

func TestGameStateJSONContainsID(t *testing.T) {
	if !GameStateJSONContainsID(`{"player":{"relics":[{"id":"CLOAK_CLASP","block_gained":5}]}}`, "CLOAK_CLASP") {
		t.Fatal("verbatim id should be present")
	}
	if !GameStateJSONContainsID(`{"id":"cloak_clasp"}`, "CLOAK_CLASP") {
		t.Fatal("id match should be case-insensitive")
	}
	if GameStateJSONContainsID(`{"id":"CLOAK_CLASP_PLUS"}`, "CLOAK_CLASP") {
		t.Fatal("longer id token must NOT match (boundary-strict)")
	}
	if GameStateJSONContainsID(`{"relics":[{"id":"AKABEKO"}]}`, "CLOAK_CLASP") {
		t.Fatal("absent id must not match")
	}
	if GameStateJSONContainsID("", "CLOAK_CLASP") {
		t.Fatal("empty json must not match")
	}
	if GameStateJSONContainsID(`{"id":"CLOAK_CLASP"}`, "") {
		t.Fatal("empty id must not match")
	}
}

func mustMatch(t *testing.T, expected, observed string) {
	t.Helper()
	if !ExpectedTextMatch(expected, observed) {
		t.Fatalf("ExpectedTextMatch(%q, %q) = false, want true", expected, observed)
	}
}

func mustNotMatch(t *testing.T, expected, observed string) {
	t.Helper()
	if ExpectedTextMatch(expected, observed) {
		t.Fatalf("ExpectedTextMatch(%q, %q) = true, want false", expected, observed)
	}
}

// trxFixture mirrors the Pester New-TrxFixture helper.
func trxFixture(total, failed int, failedNames, passedNames []string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<TestRun xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010">` + "\n")
	b.WriteString("  <Results>\n")
	for _, n := range passedNames {
		fmt.Fprintf(&b, "    <UnitTestResult testName=%q outcome=\"Passed\" />\n", n)
	}
	for _, n := range failedNames {
		fmt.Fprintf(&b, "    <UnitTestResult testName=%q outcome=\"Failed\" />\n", n)
	}
	b.WriteString("  </Results>\n")
	b.WriteString(`  <ResultSummary outcome="Completed">` + "\n")
	fmt.Fprintf(&b, "    <Counters total=%q executed=%q passed=%q failed=%q />\n",
		itoa(total), itoa(total), itoa(total-failed), itoa(failed))
	b.WriteString("  </ResultSummary>\n")
	b.WriteString("</TestRun>\n")
	return b.String()
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

func writeTRX(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.trx")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write trx: %v", err)
	}
	return path
}

func TestObservedUnitTestResult_AllPass(t *testing.T) {
	trx := writeTRX(t, trxFixture(99, 0, nil, []string{"A", "B", "C"}))
	r := ObservedUnitTestResult(0, trx)
	if !r.Passed || r.Total != 99 || r.Failed != 0 || len(r.FailedNames) != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestObservedUnitTestResult_NFailures(t *testing.T) {
	trx := writeTRX(t, trxFixture(50, 2,
		[]string{"SchemaLoadingTests.LoadsPooledShape", "PoisonTooltipTests.ShowsDownstreamDamage"},
		[]string{"X", "Y"}))
	r := ObservedUnitTestResult(1, trx)
	if r.Passed || r.Total != 50 || r.Failed != 2 {
		t.Fatalf("got %+v", r)
	}
	if !contains(r.FailedNames, "SchemaLoadingTests.LoadsPooledShape") ||
		!contains(r.FailedNames, "PoisonTooltipTests.ShowsDownstreamDamage") {
		t.Fatalf("failed names = %v", r.FailedNames)
	}
	if !strings.Contains(r.Notes, "SchemaLoadingTests.LoadsPooledShape") {
		t.Fatalf("notes should name failures: %q", r.Notes)
	}
}

func TestObservedUnitTestResult_ZeroTests(t *testing.T) {
	trx := writeTRX(t, trxFixture(0, 0, nil, nil))
	r := ObservedUnitTestResult(0, trx)
	if !r.Passed || r.Total != 0 || r.Failed != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestObservedUnitTestResult_HistoricalProseTrap(t *testing.T) {
	// "99 passed, 0 failed" maps to PASSED because failed == 0; the verdict no
	// longer depends on prose.
	trx := writeTRX(t, trxFixture(99, 0, nil, nil))
	r := ObservedUnitTestResult(0, trx)
	if !r.Passed || r.Total != 99 || r.Failed != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestObservedUnitTestResult_CleanTRXNonzeroExitFails(t *testing.T) {
	trx := writeTRX(t, trxFixture(10, 0, nil, nil))
	r := ObservedUnitTestResult(1, trx)
	if r.Passed {
		t.Fatal("clean TRX + nonzero exit must be passed=false")
	}
}

func TestObservedUnitTestResult_EnumeratedRowsOutvoteStaleCounter(t *testing.T) {
	// Summary says failed=0 but a failing row exists: the row wins.
	trx := writeTRX(t, trxFixture(5, 0, []string{"Flaky.Test"}, nil))
	r := ObservedUnitTestResult(1, trx)
	if r.Passed || r.Failed != 1 || !contains(r.FailedNames, "Flaky.Test") {
		t.Fatalf("got %+v", r)
	}
}

func TestObservedUnitTestResult_MissingTRX(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.trx")
	r := ObservedUnitTestResult(1, missing)
	if r.Passed || !strings.Contains(r.Notes, "no structured TRX") {
		t.Fatalf("missing+nonzero: %+v", r)
	}
	r = ObservedUnitTestResult(0, missing)
	if !r.Passed || !strings.Contains(r.Notes, "no structured TRX") {
		t.Fatalf("missing+zero: %+v", r)
	}
	// null/empty path handled like missing.
	if r := ObservedUnitTestResult(0, ""); !r.Passed {
		t.Fatalf("empty path zero exit should pass: %+v", r)
	}
}

func TestObservedUnitTestResult_UnparseableTRX(t *testing.T) {
	junk := writeTRX(t, "this is not xml <<<")
	r := ObservedUnitTestResult(1, junk)
	if r.Passed {
		t.Fatal("unparseable TRX + nonzero exit must be passed=false")
	}
	if !strings.Contains(r.Notes, "could not be parsed") && !strings.Contains(r.Notes, "no structured TRX") {
		t.Fatalf("notes = %q", r.Notes)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
