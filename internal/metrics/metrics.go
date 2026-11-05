// Package metrics computes CER and aggregates reading-quality and gate
// indicators. No global state: everything lives in values.
package metrics

import (
	"sort"
	"time"
)

// Levenshtein returns the edit distance between two strings counted in
// runes. Memory O(min(n,m)) with two reused rows. Fast path: when both
// strings are pure ASCII (the normalized-label case) it works on bytes
// without converting to []rune, saving two allocations per call.
func Levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	if isASCII(a) && isASCII(b) {
		return levenshteinASCII(a, b)
	}
	return levenshteinRunes([]rune(a), []rune(b))
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7F {
			return false
		}
	}
	return true
}

// levenshteinASCII is the two-row DP over bytes. It exists separately from
// the rune one because Go does not allow indexing a generic string|[]rune
// union; the duplication is deliberate in exchange for zero conversions on
// the hot path.
func levenshteinASCII(a, b string) int {
	if len(b) > len(a) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		ca := a[i-1]
		for j := 1; j <= len(b); j++ {
			cost := 0
			if ca != b[j-1] {
				cost = 1
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// levenshteinRunes is the same DP over runes for non-ASCII text.
func levenshteinRunes(a, b []rune) int {
	if len(b) > len(a) {
		a, b = b, a
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		ca := a[i-1]
		for j := 1; j <= len(b); j++ {
			cost := 0
			if ca != b[j-1] {
				cost = 1
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// CER is the character error rate: distance / reference length in runes.
// Empty reference: 0 when the hypothesis is also empty, 1 otherwise.
// It never returns NaN.
func CER(reference, hypothesis string) float64 {
	n := len([]rune(reference))
	if n == 0 {
		if hypothesis == "" {
			return 0
		}
		return 1
	}
	return float64(Levenshtein(reference, hypothesis)) / float64(n)
}

// FieldStat accumulates attempts, exact hits, and CER for one field.
type FieldStat struct {
	Attempts int     `json:"attempts"`
	Exact    int     `json:"exact"`
	CERSum   float64 `json:"-"` // accumulator only; MeanCER exposes it
}

// Observe records one reading against its already-normalized reference.
func (f *FieldStat) Observe(reference, hypothesis string) {
	f.Attempts++
	if reference == hypothesis {
		f.Exact++
	}
	f.CERSum += CER(reference, hypothesis)
}

// SuccessRate is the exact-read success rate. Zero attempts returns 0.
func (f FieldStat) SuccessRate() float64 {
	if f.Attempts == 0 {
		return 0
	}
	return float64(f.Exact) / float64(f.Attempts)
}

// MeanCER is the field mean CER. Zero attempts returns 0.
func (f FieldStat) MeanCER() float64 {
	if f.Attempts == 0 {
		return 0
	}
	return f.CERSum / float64(f.Attempts)
}

// FieldReport is the serializable view of a FieldStat.
type FieldReport struct {
	Field       string  `json:"field"`
	Attempts    int     `json:"attempts"`
	Exact       int     `json:"exact"`
	SuccessRate float64 `json:"success_rate"`
	MeanCER     float64 `json:"mean_cer"`
}

// Report converts the accumulator into its report view.
func (f FieldStat) Report(field string) FieldReport {
	return FieldReport{
		Field: field, Attempts: f.Attempts, Exact: f.Exact,
		SuccessRate: f.SuccessRate(), MeanCER: f.MeanCER(),
	}
}

// CycleStats summarizes per-box cycle times.
type CycleStats struct {
	Count int           `json:"count"`
	Mean  time.Duration `json:"mean"`
	P50   time.Duration `json:"p50"`
	P95   time.Duration `json:"p95"`
	Max   time.Duration `json:"max"`
}

// SummarizeCycles computes mean and percentiles without mutating the input.
func SummarizeCycles(durations []time.Duration) CycleStats {
	if len(durations) == 0 {
		return CycleStats{}
	}
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	return CycleStats{
		Count: len(sorted),
		Mean:  total / time.Duration(len(sorted)),
		P50:   percentile(sorted, 50),
		P95:   percentile(sorted, 95),
		Max:   sorted[len(sorted)-1],
	}
}

// percentile uses the nearest-rank method over sorted data.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p*len(sorted) + 99) / 100 // ceil(p/100 * n)
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

// Recall computes TP/(TP+FN). With no seeded cases it returns 0 (not
// "100%"): claiming recall without a denominator would be fake evidence.
func Recall(detected, seeded int) float64 {
	if seeded == 0 {
		return 0
	}
	return float64(detected) / float64(seeded)
}
