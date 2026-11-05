package metrics

import (
	"testing"
	"time"
)

func TestLevenshteinKnownDistances(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"kitten", "sitting", 3},
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"flaw", "lawn", 2},
		{"KIT", "K1T", 1},
		{"niño", "nino", 1}, // counted in runes, not bytes (accented test data)
	}
	for _, c := range cases {
		if got := Levenshtein(c.a, c.b); got != c.want {
			t.Errorf("Levenshtein(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCEREdges(t *testing.T) {
	if cer := CER("", ""); cer != 0 {
		t.Fatalf("empty/empty CER=%v want 0", cer)
	}
	if cer := CER("", "x"); cer != 1 {
		t.Fatalf("empty/x CER=%v want 1", cer)
	}
	if cer := CER("HEAVY DUTY FRONT BRAKE SERVICE KIT", "HFAVY DUTY FRONT BRAKE SERVICE KIT"); cer <= 0 || cer > 0.05 {
		t.Fatalf("one confusion in 34 chars CER=%v, want within (0, 0.05]", cer)
	}
}

func TestFieldStatRates(t *testing.T) {
	var f FieldStat
	f.Observe("ABC", "ABC")
	f.Observe("ABC", "AXC")
	if f.SuccessRate() != 0.5 {
		t.Fatalf("SuccessRate=%v want 0.5", f.SuccessRate())
	}
	if cer := f.MeanCER(); cer < 0.16 || cer > 0.17 {
		t.Fatalf("MeanCER=%v want ~1/6", cer)
	}
	var empty FieldStat
	if empty.SuccessRate() != 0 || empty.MeanCER() != 0 {
		t.Fatal("empty FieldStat must report zeros, not NaN")
	}
}

func TestSummarizeCyclesPercentiles(t *testing.T) {
	in := []time.Duration{}
	for i := 1; i <= 100; i++ {
		in = append(in, time.Duration(i)*time.Microsecond)
	}
	// Reversed input: SummarizeCycles must neither mutate it nor depend on order.
	rev := make([]time.Duration, len(in))
	for i, v := range in {
		rev[len(in)-1-i] = v
	}
	s := SummarizeCycles(rev)
	if s.Count != 100 || s.Max != 100*time.Microsecond {
		t.Fatalf("count/max wrong: %+v", s)
	}
	if s.P50 != 50*time.Microsecond || s.P95 != 95*time.Microsecond {
		t.Fatalf("percentiles wrong: p50=%v p95=%v", s.P50, s.P95)
	}
	if rev[0] != 100*time.Microsecond {
		t.Fatal("input slice was mutated")
	}
	if got := SummarizeCycles(nil); got != (CycleStats{}) {
		t.Fatalf("empty stats = %+v", got)
	}
}

func TestRecallZeroDenominatorIsZero(t *testing.T) {
	if r := Recall(0, 0); r != 0 {
		t.Fatalf("Recall(0,0)=%v; with no cases, 100%% must not be claimed", r)
	}
	if r := Recall(10, 10); r != 1 {
		t.Fatalf("Recall(10,10)=%v want 1", r)
	}
}

func BenchmarkCER(b *testing.B) {
	ref := "HEAVY DUTY FRONT BRAKE SERVICE KIT 7501234500012 L26041A"
	hyp := "HFAVY DUTY FR0NT BRAKE SERVICE KIT 7501234500012 L26041A"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = CER(ref, hyp)
	}
}

func BenchmarkLevenshteinLong(b *testing.B) {
	a := "INDUSTRIAL REUSABLE AIR FILTER ELEMENT INDUSTRIAL REUSABLE AIR FILTER ELEMENT"
	c := "1NDUSTRIAL REUSABLE AIR F1LTER ELEMENT INDUSTRIAL REUSABLE AIR FILTER ELEMENT"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Levenshtein(a, c)
	}
}
