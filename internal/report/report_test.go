package report

import (
	"strings"
	"testing"
	"time"

	"packcheck/internal/domain"
)

func caseRep(id string, expected, got domain.Verdict, seeded, critical bool, matches bool, shift string) CaseReport {
	return CaseReport{
		CaseID: id, ExpectedVerdict: expected, GotVerdict: got,
		SeededError: seeded, SeededCritical: critical, MatchesExpected: matches, Shift: shift,
	}
}

func TestBuildAggregateHandComputed(t *testing.T) {
	cases := []CaseReport{
		caseRep("good", domain.VerdictPass, domain.VerdictPass, false, false, true, "A"),
		caseRep("seeded-detected", domain.VerdictReject, domain.VerdictReject, true, true, true, "A"),
		caseRep("seeded-escaped", domain.VerdictReject, domain.VerdictPass, true, true, false, "B"),
		caseRep("false-reject", domain.VerdictPass, domain.VerdictReject, false, false, false, "B"),
	}
	a := BuildAggregate(cases, []time.Duration{time.Millisecond, 2 * time.Millisecond})
	if a.Total != 4 || a.Passed != 2 || a.Rejected != 2 {
		t.Fatalf("counts: %+v", a)
	}
	if a.CriticalRecall != 0.5 || a.OverallRecall != 0.5 {
		t.Fatalf("recalls: %+v", a)
	}
	if a.FalseRejects != 1 || a.FalseRejectsByShift["B"] != 1 {
		t.Fatalf("false rejects: %+v", a.FalseRejectsByShift)
	}
	if a.EscapesBeforeGate != 2 || a.EscapesAfterGate != 1 {
		t.Fatalf("escapes before/after: %+v", a)
	}
	if a.MatchedExpected != 2 || a.Mismatched != 2 {
		t.Fatalf("matches: %+v", a)
	}
}

func TestHTMLEscapesUntrustedText(t *testing.T) {
	r := BatchReport{
		GeneratedAt: "2026-01-01T00:00:00Z", Engine: "sim",
		Cases: []CaseReport{{
			CaseID: "xss", Description: "<script>alert(1)</script>",
			ExpectedVerdict: domain.VerdictPass, GotVerdict: domain.VerdictPass,
			Truth: Truth{ProductText: "<b>no</b>"},
		}},
	}
	r.Aggregate = BuildAggregate(r.Cases, nil)
	out, err := HTMLBytes(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "<script>alert(1)</script>") {
		t.Fatal("HTML must escape label text (XSS via report)")
	}
	if !strings.Contains(string(out), "&lt;script&gt;") {
		t.Fatal("escaped form should be present")
	}
}

func TestTextSummaryContainsGates(t *testing.T) {
	r := BatchReport{GeneratedAt: "t", Engine: "sim", Cases: []CaseReport{
		caseRep("s", domain.VerdictReject, domain.VerdictReject, true, true, true, "A"),
	}}
	r.Aggregate = BuildAggregate(r.Cases, nil)
	s := TextSummary(r)
	for _, want := range []string{"Critical recall: 100.0%", "False rejects: 0"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q\n%s", want, s)
		}
	}
}
