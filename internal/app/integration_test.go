package app

import (
	"context"
	"errors"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"packcheck/internal/domain"
	"packcheck/internal/ocr"
	"packcheck/internal/ports"
	sqlitestore "packcheck/internal/store/sqlite"
	"packcheck/internal/validation"
)

// TestEndToEndBatchAgainstSQLite walks the full flow: a real file-backed
// SQLite database, SQL seeding, PNG images, reading through the OCR port,
// validation against the SQL order, metrics, and a manifest with verified
// fingerprints.
func TestEndToEndBatchAgainstSQLite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlitestore.Open(filepath.Join(dir, "orders.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	orders, err := SeedDemo(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	// Up-front SQL cross-check: what was seeded reads back from SQLite.
	fromSQL, err := store.GetOrder(ctx, "OP-2026-0001")
	if err != nil {
		t.Fatal(err)
	}
	if fromSQL.GTIN != "7501234500012" || len(fromSQL.Contents) != 3 {
		t.Fatalf("SQL round-trip wrong: %+v", fromSQL)
	}

	cases := DemoCases(orders)
	if len(cases) != 29 {
		t.Fatalf("demo matrix size = %d, want 29", len(cases))
	}
	labelsDir := filepath.Join(dir, "labels")
	manifest, _, err := GenerateLabels(cases, labelsDir, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Every image exists, decodes as PNG, and its SHA-256 matches.
	for _, mc := range manifest.Cases {
		path := filepath.Join(labelsDir, mc.ImageFile)
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("case %s image: %v", mc.CaseID, err)
		}
		img, err := png.Decode(f)
		_ = f.Close()
		if err != nil {
			t.Fatalf("case %s PNG decode: %v", mc.CaseID, err)
		}
		if img.Bounds().Dx() != 720 || img.Bounds().Dy() != 420 {
			t.Fatalf("case %s size %v", mc.CaseID, img.Bounds())
		}
		sum, err := SHA256File(path)
		if err != nil {
			t.Fatal(err)
		}
		if sum != mc.ImageSHA256 {
			t.Fatalf("case %s sha256 manifest mismatch", mc.CaseID)
		}
	}

	validator, err := validation.NewBoxValidator(store, ports.SystemClock{}, validation.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	batch, err := RunBatch(ctx, BatchDeps{
		Engine: ocr.DeterministicEngine{}, Validator: validator, Clock: ports.SystemClock{},
	}, cases, manifest)
	if err != nil {
		t.Fatal(err)
	}

	a := batch.Aggregate
	t.Logf("aggregate: total=%d matched=%d critical_recall=%.3f false_rejects=%d mean_ms=%.4f p95_ms=%.4f",
		a.Total, a.MatchedExpected, a.CriticalRecall, a.FalseRejects, a.Cycle.MeanMs, a.Cycle.P95Ms)
	if a.Mismatched != 0 {
		for _, c := range batch.Cases {
			if !c.MatchesExpected {
				t.Errorf("case %s: expected %v/%v got %v findings=%+v",
					c.CaseID, c.ExpectedVerdict, c.ExpectedCodes, c.GotVerdict, c.Findings)
			}
		}
	}
	if a.CriticalRecall != 1.0 {
		t.Fatalf("critical recall = %v, gate is 100%%", a.CriticalRecall)
	}
	if a.OverallRecall != 1.0 {
		t.Fatalf("overall seeded recall = %v, want 100%% in the demo batch", a.OverallRecall)
	}
	if a.FalseRejects != 0 {
		t.Fatalf("false rejects = %d, gate is 0", a.FalseRejects)
	}
	if a.EscapesBeforeGate != 13 || a.EscapesAfterGate != 0 {
		t.Fatalf("escapes before=%d after=%d, want 13/0", a.EscapesBeforeGate, a.EscapesAfterGate)
	}
	if a.Cycle.P95Ms > 100 {
		t.Fatalf("cycle p95 = %.3fms, gate is < 100ms per box", a.Cycle.P95Ms)
	}
	// Cycle time via repeated measurement: on coarse-tick clocks a single
	// validation can measure 0 (verified with an independent probe in this
	// environment), so iterations accumulate until the clock advances and
	// are then divided. It is the same technique the Go benchmark harness uses.
	assertCycleGate(t, ctx, validator, orders[0])

	byField := map[string]struct {
		success, cer float64
		attempts     int
	}{}
	for _, f := range batch.Fields {
		byField[f.Field] = struct {
			success, cer float64
			attempts     int
		}{f.SuccessRate, f.MeanCER, f.Attempts}
	}
	if byField["gtin"].success != 1.0 || byField["lot"].success != 1.0 {
		t.Fatalf("critical printable fields must read exactly in simulator: %+v", byField)
	}
	if byField["text"].success >= 1.0 || byField["text"].cer <= 0 {
		t.Fatalf("degraded profiles must produce text CER > 0: %+v", byField["text"])
	}
	if byField["expiry"].success >= 1.0 {
		t.Fatal("glare case must make expiry read success < 100%")
	}
	for field, s := range byField {
		if s.attempts != 29 {
			t.Errorf("field %s attempts=%d want 29", field, s.attempts)
		}
	}
}

// assertCycleGate measures the real per-box cycle against SQLite by
// repeating validation until a measurable elapsed time is obtained (max 50k
// iterations).
func assertCycleGate(t *testing.T, ctx context.Context, validator *validation.BoxValidator, o domain.Order) {
	t.Helper()
	scan := domain.BoxScan{
		BoxID: "BOX-CYCLE", OrderID: o.ID, Shift: "A",
		Label:    domain.ManualReading(o.ProductName, o.GTIN, o.Lot, o.ExpiryDate()),
		Contents: o.Contents,
	}
	const batch = 500
	start := time.Now()
	iters := 0
	var elapsed time.Duration
	for iters < 50000 {
		for i := 0; i < batch; i++ {
			if _, err := validator.ValidateBox(ctx, scan); err != nil {
				t.Fatal(err)
			}
		}
		iters += batch
		elapsed = time.Since(start)
		if elapsed > 0 {
			break
		}
	}
	if elapsed <= 0 {
		t.Fatalf("clock never advanced in %d validations; cannot measure cycle in this environment", iters)
	}
	perOp := elapsed / time.Duration(iters)
	t.Logf("cycle gate: %d validations in %v => %v per box (SQL included)", iters, elapsed, perOp)
	if perOp > 10*time.Millisecond {
		t.Fatalf("cycle per box = %v, gate is < 10ms", perOp)
	}
}

// TestTesseractAvailabilityIsExplicit verifies the real adapter without
// faking: without the binary the error is the documented sentinel; with the
// binary it reads a generated image without asserting content (the bitmap
// font is not a real typeface for Tesseract).
func TestTesseractAvailabilityIsExplicit(t *testing.T) {
	engine, err := ocr.NewEngine("tesseract")
	if err != nil {
		if !errors.Is(err, domain.ErrTesseractUnavailable) {
			t.Fatalf("err=%v want ErrTesseractUnavailable", err)
		}
		t.Logf("tesseract not installed in this environment: %v (explicit, no fallback)", err)
		return
	}
	dir := t.TempDir()
	manifest, _, err := GenerateLabels(DemoCases(DemoOrders())[:1], filepath.Join(dir, "labels"), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	_, err = engine.Read(context.Background(), ports.OCRRequest{
		BoxID: "BOX-T",
		Image: ports.LabelImage{Path: filepath.Join(dir, "labels", manifest.Cases[0].ImageFile), Profile: ocr.ProfileClean},
	})
	if err != nil {
		t.Fatalf("tesseract present but read failed: %v", err)
	}
}

// TestValidateSingleBoxThroughSQL exercises the gate as a library, no CLI.
func TestValidateSingleBoxThroughSQL(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	orders, err := SeedDemo(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := validation.NewBoxValidator(store, ports.FixedClock{
		T: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC),
	}, validation.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	o := orders[0]
	scan := domain.BoxScan{
		BoxID: "BOX-LIB", OrderID: o.ID, Shift: "A",
		Label:    domain.ManualReading(o.ProductName, o.GTIN, "L99999X", o.ExpiryDate()),
		Contents: o.Contents,
	}
	res, err := validator.ValidateBox(ctx, scan)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != domain.VerdictReject || !res.HasCode(domain.CodeLotMismatch) {
		t.Fatalf("wrong lot through SQL must reject with LOT_MISMATCH: %+v", res)
	}
}
