package ocr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"packcheck/internal/domain"
	"packcheck/internal/ports"
)

// TesseractEngine reads real pixels by delegating to the `tesseract` binary.
// Security: it runs without a shell, with fixed arguments and a timeout; the
// only variable is the already-existing image path, passed as an argument.
type TesseractEngine struct {
	Binary  string        // resolved binary path
	Timeout time.Duration // per-read limit (default 15s)
}

// Name identifies the engine in reports.
func (TesseractEngine) Name() string { return "tesseract" }

// NewTesseractEngine resolves the binary on PATH. When missing it returns an
// error wrapping domain.ErrTesseractUnavailable: the CLI must say so
// explicitly instead of faking readings.
func NewTesseractEngine() (TesseractEngine, error) {
	bin, err := exec.LookPath("tesseract")
	if err != nil {
		return TesseractEngine{}, fmt.Errorf("%w: %v", domain.ErrTesseractUnavailable, err)
	}
	return TesseractEngine{Binary: bin, Timeout: 15 * time.Second}, nil
}

// Read runs OCR over req.Image.Path and extracts fields with strict
// patterns. Quality is unknown (this version does not parse the confidence
// TSV), so QualityKnown=false and legibility alone never blocks: the field
// validators still require exact values.
func (e TesseractEngine) Read(ctx context.Context, req ports.OCRRequest) (domain.LabelReading, error) {
	if e.Binary == "" {
		return domain.LabelReading{}, fmt.Errorf("%w: empty binary path", domain.ErrTesseractUnavailable)
	}
	if strings.TrimSpace(req.Image.Path) == "" {
		return domain.LabelReading{}, fmt.Errorf("ocr: tesseract requires an image path")
	}
	info, err := os.Stat(req.Image.Path)
	if err != nil {
		return domain.LabelReading{}, fmt.Errorf("ocr: stat image: %w", err)
	}
	if !info.Mode().IsRegular() {
		return domain.LabelReading{}, fmt.Errorf("ocr: image path is not a regular file")
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Absolute path: the one variable argument can never be parsed as a flag.
	absPath, err := filepath.Abs(req.Image.Path)
	if err != nil {
		return domain.LabelReading{}, fmt.Errorf("ocr: resolve image path: %w", err)
	}
	// #nosec G204 -- binary resolved via LookPath, no shell, fixed arguments
	// except the absolute path already validated as a regular file; timed out.
	cmd := exec.CommandContext(ctx, e.Binary, absPath, "stdout", "--psm", "6")
	out, err := cmd.Output()
	if err != nil {
		return domain.LabelReading{}, fmt.Errorf("ocr: tesseract run: %w", err)
	}
	return ParseLabelText(string(out)), nil
}

var (
	reGTIN   = regexp.MustCompile(`\b\d{13}\b`)
	reLot    = regexp.MustCompile(`(?i)\bLOT[E]?\s*[:#-]?\s*([A-Z0-9-]{4,20})\b`)
	reExpiry = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2}|\d{2}/\d{2}/\d{4}|\d{2}-\d{2}-\d{4}|\d{8}|\d{2}/\d{4}|\d{4}-\d{2})\b`)
)

// ParseLabelText extracts label fields from plain OCR text. The first
// non-empty line that is not the GTIN/lot/expiry is taken as product text.
// It is exported and pure so it can be tested without the binary installed.
func ParseLabelText(raw string) domain.LabelReading {
	reading := domain.LabelReading{QualityKnown: false}
	if m := reGTIN.FindString(raw); m != "" {
		reading.GTIN = m
	}
	if m := reLot.FindStringSubmatch(raw); len(m) == 2 {
		reading.Lot = m[1]
	}
	if m := reExpiry.FindString(raw); m != "" {
		reading.ExpiryText = m
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == reading.GTIN || line == reading.ExpiryText {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(line), "LOT") {
			continue
		}
		reading.ProductText = line
		break
	}
	return reading
}

// NewEngine is the engine factory by name ("simulator" | "tesseract").
func NewEngine(name string) (ports.OCREngine, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "simulator", "deterministic":
		return DeterministicEngine{}, nil
	case "tesseract":
		engine, err := NewTesseractEngine()
		if err != nil {
			return nil, err
		}
		return engine, nil
	default:
		return nil, fmt.Errorf("ocr: unknown engine %q (use simulator|tesseract)", name)
	}
}
