package ocr

import (
	"context"
	"errors"
	"testing"

	"packcheck/internal/domain"
	"packcheck/internal/ports"
)

var truth = ports.GroundTruth{
	ProductText: "HEAVY DUTY FRONT BRAKE SERVICE KIT",
	GTIN:        "7501234500012", Lot: "L26041A", ExpiryText: "2027-06-30",
}

func readProfile(t *testing.T, profile string) domain.LabelReading {
	t.Helper()
	r, err := DeterministicEngine{}.Read(context.Background(), ports.OCRRequest{
		BoxID: "B1", Image: ports.LabelImage{Profile: profile}, Truth: truth,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSimulatorCleanIsExactAndDeterministic(t *testing.T) {
	a := readProfile(t, ProfileClean)
	b := readProfile(t, ProfileClean)
	if a != b {
		t.Fatalf("simulator must be deterministic: %+v vs %+v", a, b)
	}
	if a.ProductText != truth.ProductText || a.GTIN != truth.GTIN ||
		a.Lot != truth.Lot || a.ExpiryText != truth.ExpiryText {
		t.Fatalf("clean reading must be exact: %+v", a)
	}
	if !a.QualityKnown {
		t.Fatal("simulator must report known quality")
	}
}

func TestSimulatorProfileQualityContract(t *testing.T) {
	// Specification contract with thresholds 0.80 / 0.70.
	for _, p := range []string{ProfileClean, ProfileFontB, ProfileSupplierB, ProfileFaint, ProfileCurved} {
		r := readProfile(t, p)
		if r.Confidence < 0.80 || r.Legibility < 0.70 {
			t.Errorf("profile %q must stay readable, got conf=%.2f leg=%.2f", p, r.Confidence, r.Legibility)
		}
	}
	for _, p := range []string{ProfileBlur, ProfileGlare} {
		r := readProfile(t, p)
		if r.Confidence >= 0.80 && r.Legibility >= 0.70 {
			t.Errorf("profile %q must be illegible by construction, got conf=%.2f leg=%.2f", p, r.Confidence, r.Legibility)
		}
	}
	// Critical fields intact under faint/curved; only the text degrades.
	for _, p := range []string{ProfileFaint, ProfileCurved} {
		r := readProfile(t, p)
		if r.GTIN != truth.GTIN || r.Lot != truth.Lot || r.ExpiryText != truth.ExpiryText {
			t.Errorf("profile %q must keep critical fields exact: %+v", p, r)
		}
		if r.ProductText == truth.ProductText {
			t.Errorf("profile %q must degrade product text", p)
		}
	}
	if r := readProfile(t, ProfileGlare); r.ExpiryText != "" {
		t.Fatalf("glare must blank the expiry, got %q", r.ExpiryText)
	}
	engine := DeterministicEngine{}
	if _, err := engine.Read(context.Background(), ports.OCRRequest{
		Image: ports.LabelImage{Profile: "x-ray"}, Truth: truth,
	}); err == nil {
		t.Fatal("unknown profile must error, not invent a reading")
	}
}

func TestParseLabelTextExtractsFields(t *testing.T) {
	raw := "HEAVY DUTY FRONT BRAKE KIT\nGTIN: 7501234500012\nLOT: L26041A\nEXP: 2027-06-30\n"
	r := ParseLabelText(raw)
	if r.GTIN != "7501234500012" || r.Lot != "L26041A" || r.ExpiryText != "2027-06-30" {
		t.Fatalf("extraction wrong: %+v", r)
	}
	if r.ProductText != "HEAVY DUTY FRONT BRAKE KIT" {
		t.Fatalf("product text wrong: %q", r.ProductText)
	}
	if r.QualityKnown {
		t.Fatal("plain tesseract text has unknown quality in this version")
	}
}

func TestNewEngineFactory(t *testing.T) {
	e, err := NewEngine("simulator")
	if err != nil || e.Name() != "deterministic-simulator" {
		t.Fatalf("simulator engine: %v %v", e, err)
	}
	if _, err := NewEngine("magic"); err == nil {
		t.Fatal("unknown engine must error")
	}
	// Tesseract: present or absent, the outcome must be explicit.
	te, err := NewEngine("tesseract")
	if err != nil && !errors.Is(err, domain.ErrTesseractUnavailable) {
		t.Fatalf("tesseract error must wrap ErrTesseractUnavailable: %v", err)
	}
	if err == nil && te.Name() != "tesseract" {
		t.Fatalf("unexpected engine %q", te.Name())
	}
}
