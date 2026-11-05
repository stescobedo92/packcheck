// Package validation implements the pre-sealing box gate. Each field has a
// FieldValidator (Strategy) and all of them run in a chain accumulating
// findings: one bad field never hides another field's problems.
package validation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"packcheck/internal/domain"
	"packcheck/internal/metrics"
	"packcheck/internal/ports"
)

// Config holds the gate thresholds. Invalid zero values are rejected by the
// constructor so the process fails at startup, not in production.
type Config struct {
	MinConfidence   float64 // minimum acceptable OCR confidence
	MinLegibility   float64 // minimum acceptable image legibility
	TextMaxMinorCER float64 // product-text CER tolerated as a warning
}

// DefaultConfig returns the thresholds from the specification.
func DefaultConfig() Config {
	return Config{MinConfidence: 0.80, MinLegibility: 0.70, TextMaxMinorCER: 0.05}
}

func (c Config) validate() error {
	for name, v := range map[string]float64{
		"MinConfidence": c.MinConfidence, "MinLegibility": c.MinLegibility,
		"TextMaxMinorCER": c.TextMaxMinorCER,
	} {
		if v < 0 || v > 1 {
			return fmt.Errorf("config: %s must be within [0,1], got %v", name, v)
		}
	}
	if c.MinConfidence == 0 || c.MinLegibility == 0 {
		return errors.New("config: MinConfidence and MinLegibility must be > 0")
	}
	return nil
}

// FieldValidator validates one aspect of the box against the order (Strategy).
type FieldValidator interface {
	Validate(order domain.Order, scan domain.BoxScan, now time.Time, cfg Config) []domain.Finding
}

// BoxValidator is the full gate. It is safe for concurrent use: it keeps no
// mutable state between calls.
type BoxValidator struct {
	repo       ports.OrderRepository
	clock      ports.Clock
	cfg        Config
	validators []FieldValidator
}

// NewBoxValidator builds the gate with the standard validator chain. It
// fails when a dependency is missing or the configuration is invalid.
func NewBoxValidator(repo ports.OrderRepository, clock ports.Clock, cfg Config) (*BoxValidator, error) {
	if repo == nil {
		return nil, errors.New("validation: order repository is required")
	}
	if clock == nil {
		return nil, errors.New("validation: clock is required")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &BoxValidator{
		repo: repo, clock: clock, cfg: cfg,
		validators: []FieldValidator{
			LegibilityValidator{}, TextValidator{}, GTINValidator{},
			ExpiryValidator{}, LotValidator{}, ContentsValidator{},
		},
	}, nil
}

// ValidateBox cross-checks a box against its order in the repository. A
// missing order is not an infrastructure error: it is a critical rejection.
func (v *BoxValidator) ValidateBox(ctx context.Context, scan domain.BoxScan) (domain.Result, error) {
	start := v.clock.Now()
	if err := scan.Validate(); err != nil {
		return domain.Result{}, err
	}
	order, err := v.repo.GetOrder(ctx, scan.OrderID)
	if errors.Is(err, domain.ErrOrderNotFound) {
		res := domain.Result{
			BoxID: scan.BoxID, OrderID: scan.OrderID, Shift: scan.Shift,
			Verdict: domain.VerdictReject,
			Findings: []domain.Finding{{
				Field: domain.FieldOrder, Code: domain.CodeOrderNotFound,
				Severity: domain.SeverityCritical,
				Message:  "production order does not exist in SQL orders table",
				Expected: "existing order", Got: scan.OrderID,
			}},
		}
		res.CycleNanos = v.clock.Now().Sub(start).Nanoseconds()
		return res, nil
	}
	if err != nil {
		return domain.Result{}, fmt.Errorf("validation: load order %q: %w", scan.OrderID, err)
	}
	return v.ValidateAgainst(scan, order, start), nil
}

// ValidateAgainst validates without touching the repository (useful in unit
// tests and in batches where the order was already loaded once).
func (v *BoxValidator) ValidateAgainst(scan domain.BoxScan, order domain.Order, start time.Time) domain.Result {
	now := domain.TruncateToDay(v.clock.Now())
	findings := make([]domain.Finding, 0, 4)
	for _, fv := range v.validators {
		findings = append(findings, fv.Validate(order, scan, now, v.cfg)...)
	}
	if findings == nil {
		findings = []domain.Finding{}
	}
	res := domain.Result{
		BoxID: scan.BoxID, OrderID: scan.OrderID, Shift: scan.Shift,
		Verdict: domain.VerdictFromFindings(findings), Findings: findings,
	}
	res.CycleNanos = v.clock.Now().Sub(start).Nanoseconds()
	if res.CycleNanos < 0 { // fixed clock in tests: zero cycle, never negative
		res.CycleNanos = 0
	}
	return res
}

// --- Per-field validators ---

// LegibilityValidator applies the "if it cannot be read, it does not pass" rule.
type LegibilityValidator struct{}

func (LegibilityValidator) Validate(_ domain.Order, scan domain.BoxScan, _ time.Time, cfg Config) []domain.Finding {
	l := scan.Label
	if !l.QualityKnown {
		return nil
	}
	if l.Confidence >= cfg.MinConfidence && l.Legibility >= cfg.MinLegibility {
		return nil
	}
	return []domain.Finding{{
		Field: domain.FieldLegibility, Code: domain.CodeLabelIllegible,
		Severity: domain.SeverityMajor,
		Message:  "label reading quality below threshold; reprint or rescan required",
		Expected: fmt.Sprintf("confidence>=%.2f legibility>=%.2f", cfg.MinConfidence, cfg.MinLegibility),
		Got:      fmt.Sprintf("confidence=%.2f legibility=%.2f", l.Confidence, l.Legibility),
	}}
}

// TextValidator compares the normalized product text. Small degradation
// (low CER) is a warning; a different product is a critical rejection.
type TextValidator struct{}

func (TextValidator) Validate(order domain.Order, scan domain.BoxScan, _ time.Time, cfg Config) []domain.Finding {
	expected := domain.NormalizeText(order.ProductName)
	got := domain.NormalizeText(scan.Label.ProductText)
	if got == "" {
		return []domain.Finding{{
			Field: domain.FieldText, Code: domain.CodeTextMissing,
			Severity: domain.SeverityCritical, Message: "product text is empty or unreadable",
			Expected: expected, Got: scan.Label.ProductText,
		}}
	}
	if got == expected {
		return nil
	}
	if cer := metrics.CER(expected, got); cer <= cfg.TextMaxMinorCER {
		return []domain.Finding{{
			Field: domain.FieldText, Code: domain.CodeTextDegraded,
			Severity: domain.SeverityMinor,
			Message:  fmt.Sprintf("product text degraded but recognizable (CER=%.3f)", cer),
			Expected: expected, Got: got,
		}}
	}
	return []domain.Finding{{
		Field: domain.FieldText, Code: domain.CodeTextMismatch,
		Severity: domain.SeverityCritical, Message: "product text does not match production order",
		Expected: expected, Got: got,
	}}
}

// GTINValidator requires a valid GTIN-13 identical to the order's. No
// tolerance: one different digit means a different product.
type GTINValidator struct{}

func (GTINValidator) Validate(order domain.Order, scan domain.BoxScan, _ time.Time, _ Config) []domain.Finding {
	got := domain.OnlyDigits(scan.Label.GTIN)
	if !domain.ValidGTIN13(got) {
		return []domain.Finding{{
			Field: domain.FieldGTIN, Code: domain.CodeGTINInvalid,
			Severity: domain.SeverityCritical,
			Message:  "GTIN is not a valid GTIN-13 (format or check digit)",
			Expected: domain.OnlyDigits(order.GTIN), Got: scan.Label.GTIN,
		}}
	}
	if got != domain.OnlyDigits(order.GTIN) {
		return []domain.Finding{{
			Field: domain.FieldGTIN, Code: domain.CodeGTINMismatch,
			Severity: domain.SeverityCritical, Message: "GTIN belongs to a different product than the order",
			Expected: domain.OnlyDigits(order.GTIN), Got: got,
		}}
	}
	return nil
}

// ExpiryValidator requires a readable expiry, equal to the order's, not expired.
type ExpiryValidator struct{}

func (ExpiryValidator) Validate(order domain.Order, scan domain.BoxScan, now time.Time, _ Config) []domain.Finding {
	expected := domain.TruncateToDay(order.Expiry)
	got, ok := domain.ParseExpiry(scan.Label.ExpiryText)
	if !ok {
		return []domain.Finding{{
			Field: domain.FieldExpiry, Code: domain.CodeExpiryUnreadable,
			Severity: domain.SeverityCritical, Message: "expiry date cannot be parsed from label",
			Expected: domain.DateOnly(expected), Got: scan.Label.ExpiryText,
		}}
	}
	var findings []domain.Finding
	if !got.Equal(expected) {
		findings = append(findings, domain.Finding{
			Field: domain.FieldExpiry, Code: domain.CodeExpiryMismatch,
			Severity: domain.SeverityCritical, Message: "expiry date does not match production order",
			Expected: domain.DateOnly(expected), Got: domain.DateOnly(got),
		})
	}
	if got.Before(now) {
		findings = append(findings, domain.Finding{
			Field: domain.FieldExpiry, Code: domain.CodeExpiryExpired,
			Severity: domain.SeverityCritical, Message: "product is already expired",
			Expected: ">= " + domain.DateOnly(now), Got: domain.DateOnly(got),
		})
	}
	return findings
}

// LotValidator requires a well-formed lot identical to the order's: it is
// the traceability key and admits no approximations.
type LotValidator struct{}

func (LotValidator) Validate(order domain.Order, scan domain.BoxScan, _ time.Time, _ Config) []domain.Finding {
	got := domain.NormalizeCode(scan.Label.Lot)
	if !domain.ValidLot(got) {
		return []domain.Finding{{
			Field: domain.FieldLot, Code: domain.CodeLotInvalid,
			Severity: domain.SeverityCritical, Message: "lot has invalid format or is unreadable",
			Expected: domain.NormalizeCode(order.Lot), Got: scan.Label.Lot,
		}}
	}
	if got != domain.NormalizeCode(order.Lot) {
		return []domain.Finding{{
			Field: domain.FieldLot, Code: domain.CodeLotMismatch,
			Severity: domain.SeverityCritical, Message: "lot does not match production order",
			Expected: domain.NormalizeCode(order.Lot), Got: got,
		}}
	}
	return nil
}

// ContentsValidator compares box contents as a code-to-quantity multiset.
// Missing items and short quantities are critical (incomplete kit); extras
// are major (the box is still not what was ordered, but the customer is not
// left short).
type ContentsValidator struct{}

func (ContentsValidator) Validate(order domain.Order, scan domain.BoxScan, _ time.Time, _ Config) []domain.Finding {
	expected := make(map[string]int, len(order.Contents))
	for _, c := range order.Contents {
		expected[domain.NormalizeCode(c.Code)] += c.Qty
	}
	got := make(map[string]int, len(scan.Contents))
	for _, c := range scan.Contents {
		got[domain.NormalizeCode(c.Code)] += c.Qty
	}
	var findings []domain.Finding
	// Order-of-the-order iteration for deterministic messages.
	for _, c := range order.Contents {
		code := domain.NormalizeCode(c.Code)
		want, have := expected[code], got[code]
		switch {
		case have == 0:
			findings = append(findings, domain.Finding{
				Field: domain.FieldContents, Code: domain.CodeContentMissing,
				Severity: domain.SeverityCritical,
				Message:  fmt.Sprintf("kit component %s is missing", code),
				Expected: fmt.Sprintf("%s x%d", code, want), Got: code + " x0",
			})
		case have < want:
			findings = append(findings, domain.Finding{
				Field: domain.FieldContents, Code: domain.CodeContentShort,
				Severity: domain.SeverityCritical,
				Message:  fmt.Sprintf("kit component %s quantity is short", code),
				Expected: fmt.Sprintf("%s x%d", code, want), Got: fmt.Sprintf("%s x%d", code, have),
			})
		case have > want:
			findings = append(findings, domain.Finding{
				Field: domain.FieldContents, Code: domain.CodeContentOver,
				Severity: domain.SeverityMajor,
				Message:  fmt.Sprintf("kit component %s quantity exceeds the order", code),
				Expected: fmt.Sprintf("%s x%d", code, want), Got: fmt.Sprintf("%s x%d", code, have),
			})
		}
	}
	// Extras: sorted by code for reproducible output.
	extras := make([]string, 0)
	for code := range got {
		if _, ok := expected[code]; !ok {
			extras = append(extras, code)
		}
	}
	for i := 1; i < len(extras); i++ { // insertion sort: short lists, no sort import for one key
		for j := i; j > 0 && extras[j] < extras[j-1]; j-- {
			extras[j], extras[j-1] = extras[j-1], extras[j]
		}
	}
	for _, code := range extras {
		findings = append(findings, domain.Finding{
			Field: domain.FieldContents, Code: domain.CodeContentExtra,
			Severity: domain.SeverityMajor,
			Message:  fmt.Sprintf("component %s is not part of the order", code),
			Expected: "not present", Got: fmt.Sprintf("%s x%d", code, got[code]),
		})
	}
	return findings
}
