// Package domain contains the pure entities and rules for packaging
// validation. It does not depend on SQL, OCR, files, or the system clock.
package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Domain and port sentinel errors. Adapters wrap them with context using
// fmt.Errorf("...: %w", err) to allow errors.Is checks.
var (
	ErrOrderNotFound        = errors.New("order not found")
	ErrInvalidOrder         = errors.New("invalid order")
	ErrInvalidScan          = errors.New("invalid box scan")
	ErrTesseractUnavailable = errors.New("tesseract binary unavailable")
	ErrUnsafePath           = errors.New("unsafe output path")
	ErrInputTooLarge        = errors.New("input too large")
)

// Severity classifies a finding. Critical and Major reject the box.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
)

// BlocksClosing reports whether the severity prevents sealing the box.
func (s Severity) BlocksClosing() bool {
	return s == SeverityCritical || s == SeverityMajor
}

// Verdict is the final outcome of validating a box.
type Verdict string

const (
	VerdictPass            Verdict = "pass"
	VerdictPassWithWarning Verdict = "pass_with_warnings"
	VerdictReject          Verdict = "reject"
)

// Field identifies the validated field. It is used as a metrics key.
type Field string

const (
	FieldOrder      Field = "order"
	FieldText       Field = "text"
	FieldGTIN       Field = "gtin"
	FieldExpiry     Field = "expiry"
	FieldLot        Field = "lot"
	FieldContents   Field = "contents"
	FieldLegibility Field = "legibility"
)

// Stable finding codes. Tests and reports depend on these values; changing
// them is a contract change.
const (
	CodeOrderNotFound    = "ORDER_NOT_FOUND"
	CodeTextMissing      = "TEXT_MISSING"
	CodeTextMismatch     = "TEXT_MISMATCH"
	CodeTextDegraded     = "TEXT_DEGRADED"
	CodeGTINInvalid      = "GTIN_INVALID"
	CodeGTINMismatch     = "GTIN_MISMATCH"
	CodeExpiryUnreadable = "EXPIRY_UNREADABLE"
	CodeExpiryMismatch   = "EXPIRY_MISMATCH"
	CodeExpiryExpired    = "EXPIRY_EXPIRED"
	CodeLotInvalid       = "LOT_INVALID"
	CodeLotMismatch      = "LOT_MISMATCH"
	CodeContentMissing   = "CONTENT_MISSING"
	CodeContentShort     = "CONTENT_SHORT"
	CodeContentExtra     = "CONTENT_EXTRA"
	CodeContentOver      = "CONTENT_OVER"
	CodeLabelIllegible   = "LABEL_ILLEGIBLE"
)

// ContentItem is a component expected in, or counted inside, the box.
type ContentItem struct {
	Code        string `json:"code"`
	Description string `json:"description,omitempty"`
	Qty         int    `json:"qty"`
}

// Order is the production order every box is validated against.
type Order struct {
	ID          string        `json:"id"`
	SKU         string        `json:"sku"`
	ProductName string        `json:"product_name"`
	GTIN        string        `json:"gtin"`
	Lot         string        `json:"lot"`
	Expiry      time.Time     `json:"expiry"` // date only, in UTC
	Contents    []ContentItem `json:"contents"`
	Supplier    string        `json:"supplier"`
	FontProfile string        `json:"font_profile"`
}

// Validate checks order invariants before it is persisted.
func (o Order) Validate() error {
	if strings.TrimSpace(o.ID) == "" || strings.TrimSpace(o.SKU) == "" ||
		strings.TrimSpace(o.ProductName) == "" {
		return fmt.Errorf("%w: id, sku and product_name are required", ErrInvalidOrder)
	}
	if !ValidGTIN13(o.GTIN) {
		return fmt.Errorf("%w: gtin %q fails GTIN-13 check digit", ErrInvalidOrder, o.GTIN)
	}
	if !ValidLot(o.Lot) {
		return fmt.Errorf("%w: lot %q has invalid format", ErrInvalidOrder, o.Lot)
	}
	if o.Expiry.IsZero() {
		return fmt.Errorf("%w: expiry is required", ErrInvalidOrder)
	}
	if len(o.Contents) == 0 {
		return fmt.Errorf("%w: at least one content item is required", ErrInvalidOrder)
	}
	seen := make(map[string]struct{}, len(o.Contents))
	for _, c := range o.Contents {
		code := NormalizeCode(c.Code)
		if code == "" || c.Qty <= 0 {
			return fmt.Errorf("%w: content code and positive qty are required", ErrInvalidOrder)
		}
		if _, dup := seen[code]; dup {
			return fmt.Errorf("%w: duplicated content code %q", ErrInvalidOrder, c.Code)
		}
		seen[code] = struct{}{}
	}
	return nil
}

// ExpiryDate returns the expiry truncated to a UTC day (storage format).
func (o Order) ExpiryDate() string { return DateOnly(o.Expiry) }

// LabelReading is what was read from the physical label of a box.
type LabelReading struct {
	ProductText  string  `json:"product_text"`
	GTIN         string  `json:"gtin"`
	Lot          string  `json:"lot"`
	ExpiryText   string  `json:"expiry_text"`
	Confidence   float64 `json:"confidence"` // 0..1
	Legibility   float64 `json:"legibility"` // 0..1
	QualityKnown bool    `json:"quality_known"`
}

// ManualReading builds a reading captured by an operator or a scanner that
// does not report quality. The legibility gate does not apply to these.
func ManualReading(productText, gtin, lot, expiryText string) LabelReading {
	return LabelReading{
		ProductText: productText, GTIN: gtin, Lot: lot, ExpiryText: expiryText,
		QualityKnown: false,
	}
}

// BoxScan is everything observed about a box before sealing it.
type BoxScan struct {
	BoxID    string        `json:"box_id"`
	OrderID  string        `json:"order_id"`
	Shift    string        `json:"shift"`
	Label    LabelReading  `json:"label"`
	Contents []ContentItem `json:"contents"`
}

// Validate checks structural scan invariants. It does not judge whether the
// box is correct; the validation package does that against the order.
func (b BoxScan) Validate() error {
	if strings.TrimSpace(b.BoxID) == "" || strings.TrimSpace(b.OrderID) == "" {
		return fmt.Errorf("%w: box_id and order_id are required", ErrInvalidScan)
	}
	if b.Label.QualityKnown {
		if b.Label.Confidence < 0 || b.Label.Confidence > 1 ||
			b.Label.Legibility < 0 || b.Label.Legibility > 1 {
			return fmt.Errorf("%w: confidence and legibility must be within [0,1]", ErrInvalidScan)
		}
	}
	for _, c := range b.Contents {
		if NormalizeCode(c.Code) == "" || c.Qty < 0 {
			return fmt.Errorf("%w: content code required and qty must be >= 0", ErrInvalidScan)
		}
	}
	return nil
}

// Finding is one concrete problem found while validating a field.
type Finding struct {
	Field    Field    `json:"field"`
	Code     string   `json:"code"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
	Expected string   `json:"expected,omitempty"`
	Got      string   `json:"got,omitempty"`
}

// Result is the full verdict for a box.
type Result struct {
	BoxID      string    `json:"box_id"`
	OrderID    string    `json:"order_id"`
	Shift      string    `json:"shift,omitempty"`
	Verdict    Verdict   `json:"verdict"`
	Findings   []Finding `json:"findings"`
	CycleNanos int64     `json:"cycle_nanos"`
}

// HasCode reports whether the result contains a finding code.
func (r Result) HasCode(code string) bool {
	for _, f := range r.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

// VerdictFromFindings derives the verdict: any blocking finding rejects;
// any minor finding passes with warnings.
func VerdictFromFindings(findings []Finding) Verdict {
	warned := false
	for _, f := range findings {
		if f.Severity.BlocksClosing() {
			return VerdictReject
		}
		warned = true
	}
	if warned {
		return VerdictPassWithWarning
	}
	return VerdictPass
}
