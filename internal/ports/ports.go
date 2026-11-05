// Package ports defines the interfaces the core needs from the outside.
// Adapters (SQLite, OCR, system clock) implement them; tests use in-memory
// or fake implementations without touching the domain.
package ports

import (
	"context"
	"time"

	"packcheck/internal/domain"
)

// OrderRepository persists and retrieves production orders. GetOrder must
// return an error wrapping domain.ErrOrderNotFound when the order is missing.
type OrderRepository interface {
	UpsertOrder(ctx context.Context, order domain.Order) error
	GetOrder(ctx context.Context, id string) (domain.Order, error)
	ListOrders(ctx context.Context) ([]domain.Order, error)
}

// LabelImage references the physical image of an already generated or
// captured label, with the profile it must be read under.
type LabelImage struct {
	Path    string // PNG path; may be empty for the simulator with direct truth
	Profile string // clean, blur, glare, curved, faint, font-b, supplier-b...
}

// GroundTruth is what the label actually says. Only the deterministic
// simulator consumes it; a real OCR engine ignores it and reads pixels.
// It exists so metrics tests are reproducible without hardware.
type GroundTruth struct {
	ProductText string
	GTIN        string
	Lot         string
	ExpiryText  string
}

// OCRRequest is the input of an OCR engine.
type OCRRequest struct {
	BoxID string
	Image LabelImage
	Truth GroundTruth
}

// OCREngine reads a label. Implementations: a deterministic simulator for
// reproducible tests and an optional Tesseract adapter for real pixels.
type OCREngine interface {
	Read(ctx context.Context, req OCRRequest) (domain.LabelReading, error)
	Name() string
}

// Clock abstracts time so cycle measurement and expiry checks are
// deterministic in tests.
type Clock interface {
	Now() time.Time
}

// SystemClock is the real clock.
type SystemClock struct{}

// Now returns the current instant preserving the monotonic clock: cycle
// time differences must be measured with it. Callers that need the civil
// date (expiries) convert with TruncateToDay/DateOnly.
func (SystemClock) Now() time.Time { return time.Now() }

// FixedClock always returns the same instant. For tests.
type FixedClock struct{ T time.Time }

// Now returns the configured fixed instant.
func (c FixedClock) Now() time.Time { return c.T }
