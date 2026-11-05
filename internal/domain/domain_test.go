package domain

import (
	"testing"
	"time"
)

func TestGTINCheckDigitKnownValue(t *testing.T) {
	// Hand-computed: weighted sum (x3, x1 from the right) = 58,
	// check digit = (10 - 58%10) % 10 = 2.
	if got := WithCheckDigit("750123450001"); got != "7501234500012" {
		t.Fatalf("WithCheckDigit = %q, want 7501234500012", got)
	}
	if !ValidGTIN13("7501234500012") {
		t.Fatal("7501234500012 must be a valid GTIN-13")
	}
}

func TestValidGTIN13RejectsTampering(t *testing.T) {
	cases := []string{
		"7501234500013", // tampered check digit
		"750123450001",  // 12 digits
		"75012345000123",
		"7501234500O12", // letter O instead of zero (typical OCR confusion)
		"",
		"abcdefghijklm",
	}
	for _, c := range cases {
		if ValidGTIN13(c) {
			t.Errorf("ValidGTIN13(%q) = true, want false", c)
		}
	}
}

func TestParseExpiryFormats(t *testing.T) {
	want := time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC)
	for _, s := range []string{"2027-06-30", "30/06/2027", "30-06-2027", "20270630", "EXP: 2027-06-30", "06/2027", "2027-06"} {
		got, ok := ParseExpiry(s)
		if !ok || !got.Equal(want) {
			t.Errorf("ParseExpiry(%q) = %v, %v; want %v, true", s, got, ok, want)
		}
	}
	for _, s := range []string{"", "##/##/####", "2027-13-40", "ayer", "32/01/2027"} {
		if _, ok := ParseExpiry(s); ok {
			t.Errorf("ParseExpiry(%q) ok=true, want false", s)
		}
	}
}

func TestNormalizeTextFoldsAccentsCaseAndPunctuation(t *testing.T) {
	if got := NormalizeText("  Heavy-Duty Brake Kit — Reinforced!  "); got != "HEAVYDUTY BRAKE KIT REINFORCED" {
		t.Fatalf("got %q", got)
	}
	// Accented Spanish/German words are intentional test data for accent folding.
	if got := NormalizeText("Piñón Ümlaut"); got != "PINON UMLAUT" {
		t.Fatalf("got %q", got)
	}
}

func TestValidLot(t *testing.T) {
	for _, okLot := range []string{"L26041A", "L-2026-001", "ABCD", "1234"} {
		if !ValidLot(okLot) {
			t.Errorf("ValidLot(%q) = false, want true", okLot)
		}
	}
	for _, bad := range []string{"", "L26", "-L26041A", "L26 041", "*********************", "LOTECONDEMASIADOSCARACTERES1"} {
		if ValidLot(bad) {
			t.Errorf("ValidLot(%q) = true, want false", bad)
		}
	}
}

func TestOrderValidateRejectsInvalid(t *testing.T) {
	base := Order{
		ID: "OP-1", SKU: "S1", ProductName: "P", GTIN: WithCheckDigit("750123450001"),
		Lot: "L0001A", Expiry: time.Now(), Contents: []ContentItem{{Code: "A", Qty: 1}},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid order rejected: %v", err)
	}
	bad := base
	bad.GTIN = "7501234500013"
	if err := bad.Validate(); err == nil {
		t.Fatal("order with bad GTIN check digit must be rejected")
	}
	bad = base
	bad.Contents = []ContentItem{{Code: "A", Qty: 1}, {Code: "a", Qty: 2}}
	if err := bad.Validate(); err == nil {
		t.Fatal("order with duplicated content code must be rejected")
	}
}

func TestBoxScanValidateQualityRange(t *testing.T) {
	scan := BoxScan{BoxID: "B1", OrderID: "OP-1",
		Label: LabelReading{QualityKnown: true, Confidence: 1.5, Legibility: 0.5}}
	if err := scan.Validate(); err == nil {
		t.Fatal("confidence > 1 must be rejected")
	}
	scan.Label = ManualReading("P", "1", "L", "E")
	if err := scan.Validate(); err != nil {
		t.Fatalf("manual reading without quality must pass: %v", err)
	}
}

func TestVerdictFromFindings(t *testing.T) {
	if v := VerdictFromFindings(nil); v != VerdictPass {
		t.Fatalf("empty = %v, want pass", v)
	}
	if v := VerdictFromFindings([]Finding{{Severity: SeverityMinor}}); v != VerdictPassWithWarning {
		t.Fatalf("minor = %v", v)
	}
	if v := VerdictFromFindings([]Finding{{Severity: SeverityMajor}}); v != VerdictReject {
		t.Fatalf("major = %v", v)
	}
}
