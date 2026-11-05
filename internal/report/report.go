// Package report produces batch evidence in three formats with the same
// content: JSON for automated systems, HTML and plain text for humans. The
// HTML uses html/template (automatic escaping) and references images in the
// labels/ subfolder next to the report.
package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"packcheck/internal/domain"
	"packcheck/internal/metrics"
)

// Truth is the ground truth printed on a case label.
type Truth struct {
	ProductText string `json:"product_text"`
	GTIN        string `json:"gtin"`
	Lot         string `json:"lot"`
	ExpiryText  string `json:"expiry_text"`
}

// CaseReport is the complete, auditable outcome of one batch case.
type CaseReport struct {
	CaseID          string              `json:"case_id"`
	Description     string              `json:"description"`
	BoxID           string              `json:"box_id"`
	OrderID         string              `json:"order_id"`
	Shift           string              `json:"shift"`
	Profile         string              `json:"profile"`
	ImageFile       string              `json:"image_file"`
	ImageSHA256     string              `json:"image_sha256"`
	Truth           Truth               `json:"truth"`
	Reading         domain.LabelReading `json:"reading"`
	ExpectedVerdict domain.Verdict      `json:"expected_verdict"`
	GotVerdict      domain.Verdict      `json:"got_verdict"`
	ExpectedCodes   []string            `json:"expected_codes"`
	Findings        []domain.Finding    `json:"findings"`
	MatchesExpected bool                `json:"matches_expected"`
	SeededError     bool                `json:"seeded_error"`
	SeededCritical  bool                `json:"seeded_critical"`
	CycleNanos      int64               `json:"cycle_nanos"`
}

// CycleReport also expresses cycle time in milliseconds for human reading.
type CycleReport struct {
	Count  int     `json:"count"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	MaxMs  float64 `json:"max_ms"`
}

func cycleReport(durations []time.Duration) CycleReport {
	s := metrics.SummarizeCycles(durations)
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	return CycleReport{Count: s.Count, MeanMs: ms(s.Mean), P50Ms: ms(s.P50), P95Ms: ms(s.P95), MaxMs: ms(s.Max)}
}

// Aggregate summarizes the whole batch with the specification gates.
type Aggregate struct {
	Total                    int            `json:"total"`
	Passed                   int            `json:"passed"`   // pass + pass_with_warnings
	Rejected                 int            `json:"rejected"` // reject
	MatchedExpected          int            `json:"matched_expected"`
	Mismatched               int            `json:"mismatched"`
	SeededErrors             int            `json:"seeded_errors"`
	SeededDetected           int            `json:"seeded_detected"`
	OverallRecall            float64        `json:"overall_recall"`
	SeededCritical           int            `json:"seeded_critical"`
	SeededCriticalDetected   int            `json:"seeded_critical_detected"`
	CriticalRecall           float64        `json:"critical_recall"`
	FalseRejects             int            `json:"false_rejects"`
	FalseRejectsByShift      map[string]int `json:"false_rejects_by_shift"`
	EscapesBeforeGate        int            `json:"escapes_before_gate"` // seeded criticals (without a gate, all ship)
	EscapesAfterGate         int            `json:"escapes_after_gate"`  // seeded criticals not rejected
	SimulatedReturnsPer1000B float64        `json:"simulated_returns_per_1000_before"`
	SimulatedReturnsPer1000A float64        `json:"simulated_returns_per_1000_after"`
	Cycle                    CycleReport    `json:"cycle"`
}

// BatchReport is the root evidence document of a batch.
type BatchReport struct {
	GeneratedAt string                `json:"generated_at"`
	Engine      string                `json:"ocr_engine"`
	Fields      []metrics.FieldReport `json:"fields"`
	Aggregate   Aggregate             `json:"aggregate"`
	Cases       []CaseReport          `json:"cases"`
}

// BuildAggregate derives the summary from the cases. Pure, tested function:
// the operational definitions live here, in exactly one place.
func BuildAggregate(cases []CaseReport, durations []time.Duration) Aggregate {
	agg := Aggregate{Total: len(cases), FalseRejectsByShift: map[string]int{}, Cycle: cycleReport(durations)}
	for _, c := range cases {
		if c.GotVerdict == domain.VerdictReject {
			agg.Rejected++
		} else {
			agg.Passed++
		}
		if c.MatchesExpected {
			agg.MatchedExpected++
		} else {
			agg.Mismatched++
		}
		if c.SeededError {
			agg.SeededErrors++
			if c.GotVerdict == domain.VerdictReject {
				agg.SeededDetected++
			}
		}
		if c.SeededCritical {
			agg.SeededCritical++
			if c.GotVerdict == domain.VerdictReject {
				agg.SeededCriticalDetected++
			}
		}
		// False reject: the truth said pass and the system rejected.
		if c.ExpectedVerdict != domain.VerdictReject && c.GotVerdict == domain.VerdictReject {
			agg.FalseRejects++
			shift := c.Shift
			if shift == "" {
				shift = "(no shift)"
			}
			agg.FalseRejectsByShift[shift]++
		}
	}
	agg.OverallRecall = metrics.Recall(agg.SeededDetected, agg.SeededErrors)
	agg.CriticalRecall = metrics.Recall(agg.SeededCriticalDetected, agg.SeededCritical)
	agg.EscapesBeforeGate = agg.SeededCritical
	agg.EscapesAfterGate = agg.SeededCritical - agg.SeededCriticalDetected
	if agg.Total > 0 {
		agg.SimulatedReturnsPer1000B = float64(agg.EscapesBeforeGate) / float64(agg.Total) * 1000
		agg.SimulatedReturnsPer1000A = float64(agg.EscapesAfterGate) / float64(agg.Total) * 1000
	}
	return agg
}

// ManifestCase is the verifiable entry of a case before it runs.
type ManifestCase struct {
	CaseID          string         `json:"case_id"`
	Description     string         `json:"description"`
	OrderID         string         `json:"order_id"`
	BoxID           string         `json:"box_id"`
	Shift           string         `json:"shift"`
	Profile         string         `json:"profile"`
	ImageFile       string         `json:"image_file"`
	ImageSHA256     string         `json:"image_sha256"`
	Truth           Truth          `json:"truth"`
	ExpectedVerdict domain.Verdict `json:"expected_verdict"`
	ExpectedCodes   []string       `json:"expected_codes"`
	SeededError     bool           `json:"seeded_error"`
	SeededCritical  bool           `json:"seeded_critical"`
}

// Manifest lists cases and images with their SHA-256 fingerprints.
type Manifest struct {
	GeneratedAt string         `json:"generated_at"`
	Cases       []ManifestCase `json:"cases"`
}

// WriteJSON serializes with stable indentation (diff-friendly).
func WriteJSON(w io.Writer, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("report: marshal json: %w", err)
	}
	if _, err := w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("report: write json: %w", err)
	}
	return nil
}

// WriteFile writes dir/name with flat-name confinement and 0600 permissions.
func WriteFile(dir, name string, content []byte) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: %q", domain.ErrUnsafePath, name)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("report: create dir: %w", err)
	}
	// Confinement via os.OpenRoot: the write cannot escape dir.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("report: open root: %w", err)
	}
	defer root.Close()
	f, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("report: create file: %w", err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("report: write file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("report: close file: %w", err)
	}
	return filepath.Join(dir, name), nil
}

// JSONBytes returns the JSON of the batch report.
func JSONBytes(r BatchReport) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ManifestJSONBytes returns the JSON of the manifest.
func ManifestJSONBytes(m Manifest) ([]byte, error) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// TextSummary is the human-readable summary for terminal and summary.txt.
func TextSummary(r BatchReport) string {
	a := r.Aggregate
	var b strings.Builder
	fmt.Fprintf(&b, "PackCheck - batch summary\nGenerated: %s | OCR engine: %s\n\n", r.GeneratedAt, r.Engine)
	fmt.Fprintf(&b, "Cases: %d | Passed: %d | Rejected: %d | Matching expectations: %d/%d\n",
		a.Total, a.Passed, a.Rejected, a.MatchedExpected, a.Total)
	fmt.Fprintf(&b, "Seeded-error recall: %.1f%% (%d/%d) | Critical recall: %.1f%% (%d/%d)\n",
		a.OverallRecall*100, a.SeededDetected, a.SeededErrors,
		a.CriticalRecall*100, a.SeededCriticalDetected, a.SeededCritical)
	fmt.Fprintf(&b, "False rejects: %d", a.FalseRejects)
	if len(a.FalseRejectsByShift) > 0 {
		shifts := make([]string, 0, len(a.FalseRejectsByShift))
		for s := range a.FalseRejectsByShift {
			shifts = append(shifts, s)
		}
		sort.Strings(shifts)
		parts := make([]string, 0, len(shifts))
		for _, s := range shifts {
			parts = append(parts, fmt.Sprintf("%s=%d", s, a.FalseRejectsByShift[s]))
		}
		fmt.Fprintf(&b, " (by shift: %s)", strings.Join(parts, ", "))
	}
	fmt.Fprintf(&b, "\nCritical escapes without gate: %d | with gate: %d\n", a.EscapesBeforeGate, a.EscapesAfterGate)
	fmt.Fprintf(&b, "Simulated returns per 1000 boxes (demo batch): before=%.1f after=%.1f\n",
		a.SimulatedReturnsPer1000B, a.SimulatedReturnsPer1000A)
	fmt.Fprintf(&b, "Cycle per box: mean=%.3fms p50=%.3fms p95=%.3fms max=%.3fms\n\n",
		a.Cycle.MeanMs, a.Cycle.P50Ms, a.Cycle.P95Ms, a.Cycle.MaxMs)
	b.WriteString("Per-field reading (against the printed label):\n")
	for _, f := range r.Fields {
		fmt.Fprintf(&b, "  %-8s success=%.1f%% meanCER=%.4f attempts=%d exact=%d\n",
			f.Field, f.SuccessRate*100, f.MeanCER, f.Attempts, f.Exact)
	}
	b.WriteString("\nCases:\n")
	for _, c := range r.Cases {
		mark := "OK  "
		if !c.MatchesExpected {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %-32s expected=%-18s got=%-18s image=%s\n",
			mark, c.CaseID, c.ExpectedVerdict, c.GotVerdict, c.ImageFile)
	}
	return b.String()
}

const htmlSource = `<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8">
<title>PackCheck - Batch evidence</title>
<style>
body{font-family:system-ui,sans-serif;margin:2rem;color:#111}
table{border-collapse:collapse;margin:1rem 0}td,th{border:1px solid #ccc;padding:.4rem .6rem;text-align:left}
.pass{color:#067647;font-weight:700}.reject{color:#b42318;font-weight:700}.fail{background:#fef3f2}
img{max-width:360px;border:1px solid #ccc}code{background:#f2f4f7;padding:.1rem .3rem}
</style></head><body>
<h1>PackCheck - Batch evidence</h1>
<p>Generated: {{.GeneratedAt}} | OCR engine: <code>{{.Engine}}</code></p>
<h2>Summary</h2>
<table>
<tr><th>Cases</th><td>{{.Aggregate.Total}}</td><th>Matching expectations</th><td>{{.Aggregate.MatchedExpected}}/{{.Aggregate.Total}}</td></tr>
<tr><th>Seeded critical recall</th><td>{{printf "%.1f%%" .CriticalRecallPct}}</td><th>False rejects</th><td>{{.Aggregate.FalseRejects}}</td></tr>
<tr><th>Critical escapes without gate</th><td>{{.Aggregate.EscapesBeforeGate}}</td><th>Escapes with gate</th><td>{{.Aggregate.EscapesAfterGate}}</td></tr>
<tr><th>Mean cycle per box</th><td>{{printf "%.3f ms" .Aggregate.Cycle.MeanMs}}</td><th>Cycle p95</th><td>{{printf "%.3f ms" .Aggregate.Cycle.P95Ms}}</td></tr>
</table>
<h2>Per-field reading (against the printed label)</h2>
<table><tr><th>Field</th><th>Success</th><th>Mean CER</th><th>Attempts</th></tr>
{{range .Fields}}<tr><td>{{.Field}}</td><td>{{printf "%.1f%%" (mul .SuccessRate 100.0)}}</td><td>{{printf "%.4f" .MeanCER}}</td><td>{{.Attempts}}</td></tr>
{{end}}</table>
<h2>Cases</h2>
{{range .Cases}}
<section class="{{if not .MatchesExpected}}fail{{end}}">
<h3>{{.CaseID}} - {{.Description}}</h3>
<p>Expected: <span class="{{.ExpectedVerdict}}">{{.ExpectedVerdict}}</span> |
Got: <span class="{{.GotVerdict}}">{{.GotVerdict}}</span> |
Matches: <strong>{{.MatchesExpected}}</strong> | Shift: {{.Shift}} | Profile: <code>{{.Profile}}</code></p>
<p>Image SHA-256: <code>{{.ImageSHA256}}</code></p>
<img src="labels/{{.ImageFile}}" alt="Label {{.CaseID}}">
<table><tr><th>Field</th><th>Printed (truth)</th><th>Read</th></tr>
<tr><td>Text</td><td>{{.Truth.ProductText}}</td><td>{{.Reading.ProductText}}</td></tr>
<tr><td>GTIN</td><td>{{.Truth.GTIN}}</td><td>{{.Reading.GTIN}}</td></tr>
<tr><td>Lot</td><td>{{.Truth.Lot}}</td><td>{{.Reading.Lot}}</td></tr>
<tr><td>Expiry</td><td>{{.Truth.ExpiryText}}</td><td>{{.Reading.ExpiryText}}</td></tr></table>
{{if .Findings}}<ul>{{range .Findings}}<li><code>{{.Code}}</code> ({{.Severity}}): {{.Message}} [expected: {{.Expected}} | got: {{.Got}}]</li>{{end}}</ul>{{else}}<p>No findings.</p>{{end}}
</section>
{{end}}
</body></html>`

type htmlView struct {
	BatchReport
	CriticalRecallPct float64
}

// HTMLBytes renders the full human report with linked images.
func HTMLBytes(r BatchReport) ([]byte, error) {
	view := htmlView{BatchReport: r, CriticalRecallPct: r.Aggregate.CriticalRecall * 100}
	funcs := template.FuncMap{"mul": func(a, b float64) float64 { return a * b }}
	t, err := template.New("report").Funcs(funcs).Parse(htmlSource)
	if err != nil {
		return nil, fmt.Errorf("report: parse html template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, view); err != nil {
		return nil, fmt.Errorf("report: render html: %w", err)
	}
	return buf.Bytes(), nil
}
