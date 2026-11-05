// Package ocr implements the OCREngine port with two adapters: a deterministic
// simulator for reproducible tests and optional Tesseract for real pixels.
// The simulator does NOT read images: it models typical OCR errors from the
// ground truth and the degradation profile. That distinction is part of the
// contract and is documented so simulation is never sold as real reading.
package ocr

import (
	"context"
	"fmt"
	"strings"

	"packcheck/internal/domain"
	"packcheck/internal/ports"
)

// Degradation profiles accepted by the simulator and the generator.
const (
	ProfileClean     = "clean"
	ProfileFaint     = "faint"
	ProfileCurved    = "curved"
	ProfileBlur      = "blur"
	ProfileGlare     = "glare"
	ProfileFontB     = "font-b"
	ProfileSupplierB = "supplier-b"
)

// ProfileSpec describes the fixed effect of a profile. The values are part of
// the specification: they determine by construction which cases pass (high
// confidence, mild degradation only in product text) and which must be
// rejected as illegible (confidence below threshold, or a field blanked by glare).
type ProfileSpec struct {
	Confidence float64
	Legibility float64
	// TextConfusions is the exact number of confusions applied ONLY to the
	// product text (the corroborative field with CER tolerance). Critical
	// fields are never "corrected" or degraded in passing profiles.
	TextConfusions int
	// BlankExpiry simulates a glare zone covering the expiry date.
	BlankExpiry bool
}

// SpecFor returns the specification of a profile. Unknown profiles return
// ok=false so callers fail explicitly instead of inventing degradations.
func SpecFor(profile string) (ProfileSpec, bool) {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "", ProfileClean:
		return ProfileSpec{Confidence: 0.99, Legibility: 0.98}, true
	case ProfileFontB, ProfileSupplierB:
		// A different well-printed font/supplier: clean reading.
		return ProfileSpec{Confidence: 0.97, Legibility: 0.95}, true
	case ProfileFaint:
		return ProfileSpec{Confidence: 0.84, Legibility: 0.78, TextConfusions: 1}, true
	case ProfileCurved:
		return ProfileSpec{Confidence: 0.83, Legibility: 0.76, TextConfusions: 1}, true
	case ProfileBlur:
		return ProfileSpec{Confidence: 0.68, Legibility: 0.55, TextConfusions: 3}, true
	case ProfileGlare:
		return ProfileSpec{Confidence: 0.51, Legibility: 0.48, TextConfusions: 1, BlankExpiry: true}, true
	default:
		return ProfileSpec{}, false
	}
}

// confusable substitutes characters OCR frequently mixes up. It is applied
// only to the product text inside the simulator.
var confusable = map[rune]rune{
	'O': '0', '0': 'O', 'I': '1', 'S': '5', 'B': '8', 'G': '6', 'E': 'F', 'A': '4',
}

// DeterministicEngine is the reproducible simulator. No randomness: it
// applies exactly the profile's number of confusions, left to right over the
// first available confusable positions. Same input, same output, on any machine.
type DeterministicEngine struct{}

// Name identifies the engine in reports.
func (DeterministicEngine) Name() string { return "deterministic-simulator" }

// Read models the reading for the profile declared in the request.
func (DeterministicEngine) Read(_ context.Context, req ports.OCRRequest) (domain.LabelReading, error) {
	spec, ok := SpecFor(req.Image.Profile)
	if !ok {
		return domain.LabelReading{}, fmt.Errorf("ocr: unknown degradation profile %q", req.Image.Profile)
	}
	text := applyTextConfusions(req.Truth.ProductText, spec.TextConfusions)
	expiry := req.Truth.ExpiryText
	if spec.BlankExpiry {
		expiry = ""
	}
	return domain.LabelReading{
		ProductText:  text,
		GTIN:         req.Truth.GTIN,
		Lot:          req.Truth.Lot,
		ExpiryText:   expiry,
		Confidence:   spec.Confidence,
		Legibility:   spec.Legibility,
		QualityKnown: true,
	}, nil
}

func applyTextConfusions(text string, n int) string {
	if n <= 0 {
		return text
	}
	runes := []rune(text)
	applied := 0
	for i, r := range runes {
		if applied >= n {
			break
		}
		if sub, ok := confusable[upperASCII(r)]; ok {
			runes[i] = sub
			applied++
		}
	}
	return string(runes)
}

func upperASCII(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - ('a' - 'A')
	}
	return r
}
