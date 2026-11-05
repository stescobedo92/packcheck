// Command packcheck is the packaging validation gate that runs before a box
// is sealed. Subcommands: seed, genlabels, validate, runbatch, version.
//
// Exit codes: 0 ok; 1 usage or internal error; 2 at least one box rejected
// in validate; 3 the batch does not match expectations in runbatch.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"packcheck/internal/app"
	"packcheck/internal/domain"
	"packcheck/internal/ocr"
	"packcheck/internal/ports"
	"packcheck/internal/report"
	sqlitestore "packcheck/internal/store/sqlite"
	"packcheck/internal/validation"
)

const (
	version      = "packcheck 1.0.0"
	maxInputSize = 10 << 20 // 10 MiB hard limit for input JSON
	exitOK       = 0
	exitError    = 1
	exitRejected = 2
	exitMismatch = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitError
	}
	ctx := context.Background()
	var err error
	var code int
	switch args[0] {
	case "seed":
		code, err = cmdSeed(ctx, args[1:], stdout, stderr)
	case "genlabels":
		code, err = cmdGenLabels(ctx, args[1:], stdout, stderr)
	case "validate":
		code, err = cmdValidate(ctx, args[1:], stdout, stderr)
	case "runbatch":
		code, err = cmdRunBatch(ctx, args[1:], stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, version)
		return exitOK
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "packcheck: unknown command %q\n\n", args[0])
		usage(stderr)
		return exitError
	}
	if err != nil {
		fmt.Fprintf(stderr, "packcheck: %v\n", err)
		if code == exitOK {
			code = exitError
		}
	}
	return code
}

func usage(w io.Writer) {
	fmt.Fprint(w, `PackCheck - label and box-contents validation before sealing the box

Usage:
  packcheck seed      --db <path.db>
  packcheck genlabels --db <path.db> --out <dir>
  packcheck validate  --db <path.db> --input <scans.json> [--out <results.json>]
  packcheck runbatch  --db <path.db> --out <dir> [--engine simulator|tesseract]
  packcheck version

Exit codes: 0 ok | 1 error | 2 box rejected (validate) | 3 batch does not match expectations (runbatch)
`)
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func openStore(path string) (*sqlitestore.Store, error) {
	if path == "" {
		return nil, errors.New("flag --db is required")
	}
	// The directory must exist: creating it is an explicit part of the local flow.
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	store, err := sqlitestore.Open(path)
	if err != nil {
		return nil, err
	}
	return store, nil
}

func newValidator(store ports.OrderRepository) (*validation.BoxValidator, error) {
	v, err := validation.NewBoxValidator(store, ports.SystemClock{}, validation.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return v, nil
}

func cmdSeed(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("seed", stderr)
	dbPath := fs.String("db", "", "SQLite database path")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	store, err := openStore(*dbPath)
	if err != nil {
		return exitError, err
	}
	defer store.Close()
	orders, err := app.SeedDemo(ctx, store)
	if err != nil {
		return exitError, err
	}
	fmt.Fprintf(stdout, "seed ok: %d orders in %s\n", len(orders), *dbPath)
	for _, o := range orders {
		fmt.Fprintf(stdout, "  %s | %s | GTIN %s | Lot %s | Exp %s | items=%d\n",
			o.ID, o.ProductName, o.GTIN, o.Lot, o.ExpiryDate(), len(o.Contents))
	}
	return exitOK, nil
}

func cmdGenLabels(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("genlabels", stderr)
	dbPath := fs.String("db", "", "SQLite database path")
	outDir := fs.String("out", "", "output directory (images in labels/ and manifest.json)")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	if *outDir == "" {
		return exitError, errors.New("flag --out is required")
	}
	store, err := openStore(*dbPath)
	if err != nil {
		return exitError, err
	}
	defer store.Close()
	orders, err := ensureOrders(ctx, store)
	if err != nil {
		return exitError, err
	}
	manifest, err := generateManifest(orders, *outDir)
	if err != nil {
		return exitError, err
	}
	fmt.Fprintf(stdout, "genlabels ok: %d images in %s\n", len(manifest.Cases), filepath.Join(*outDir, "labels"))
	for _, c := range manifest.Cases {
		fmt.Fprintf(stdout, "  %s sha256=%s\n", c.ImageFile, c.ImageSHA256)
	}
	return exitOK, nil
}

// ensureOrders upserts the canonical demo orders into the database and
// returns them. Seeding is idempotent, and re-syncing unconditionally keeps
// the SQL cross-check consistent with the demo truths even when the .db file
// was created by an older version, so every command works end to end with no
// hidden steps.
func ensureOrders(ctx context.Context, store *sqlitestore.Store) ([]domain.Order, error) {
	return app.SeedDemo(ctx, store)
}

func generateManifest(orders []domain.Order, outDir string) (report.Manifest, error) {
	cases := app.DemoCases(orders)
	manifest, _, err := app.GenerateLabels(cases, filepath.Join(outDir, "labels"), time.Now().UTC())
	if err != nil {
		return report.Manifest{}, err
	}
	data, err := report.ManifestJSONBytes(manifest)
	if err != nil {
		return report.Manifest{}, err
	}
	if _, err := report.WriteFile(outDir, "manifest.json", data); err != nil {
		return report.Manifest{}, err
	}
	return manifest, nil
}

// scansFile is the JSON shape accepted by validate (one or many scans).
type scansFile struct {
	Scans []domain.BoxScan `json:"scans"`
}

func cmdValidate(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("validate", stderr)
	dbPath := fs.String("db", "", "SQLite database path")
	input := fs.String("input", "", "JSON file with {\"scans\":[...]} (or a single scan object)")
	outPath := fs.String("out", "", "JSON output path (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	if *input == "" {
		return exitError, errors.New("flag --input is required")
	}
	scans, err := readScans(*input)
	if err != nil {
		return exitError, err
	}
	store, err := openStore(*dbPath)
	if err != nil {
		return exitError, err
	}
	defer store.Close()
	validator, err := newValidator(store)
	if err != nil {
		return exitError, err
	}
	results := make([]domain.Result, 0, len(scans))
	rejected := false
	for _, scan := range scans {
		res, err := validator.ValidateBox(ctx, scan)
		if err != nil {
			return exitError, err
		}
		if res.Verdict == domain.VerdictReject {
			rejected = true
		}
		results = append(results, res)
	}
	data, err := json.MarshalIndent(map[string]any{"results": results}, "", "  ")
	if err != nil {
		return exitError, err
	}
	data = append(data, '\n')
	if *outPath != "" {
		if err := writeOutFile(*outPath, data); err != nil {
			return exitError, err
		}
		fmt.Fprintf(stdout, "validate: %d boxes, results written to %s\n", len(results), *outPath)
	} else {
		if _, err := stdout.Write(data); err != nil {
			return exitError, err
		}
	}
	if rejected {
		return exitRejected, nil
	}
	return exitOK, nil
}

func readScans(path string) ([]domain.BoxScan, error) {
	// #nosec G304 -- local CLI: --input is an explicit operator path on their
	// own machine; the read is capped at maxInputSize via LimitReader.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open input: %w", err)
	}
	defer f.Close()
	// LimitReader+1 detects files over the limit without loading them whole.
	data, err := io.ReadAll(io.LimitReader(f, maxInputSize+1))
	if err != nil {
		return nil, fmt.Errorf("read input: %w", err)
	}
	if len(data) > maxInputSize {
		return nil, fmt.Errorf("%w: limit is %d bytes", domain.ErrInputTooLarge, maxInputSize)
	}
	var file scansFile
	dec := json.Unmarshal(data, &file)
	if dec != nil {
		// Also accept a single scan as the root object.
		var single domain.BoxScan
		if err2 := json.Unmarshal(data, &single); err2 != nil {
			return nil, fmt.Errorf("parse input json: %w", dec)
		}
		return []domain.BoxScan{single}, nil
	}
	if len(file.Scans) == 0 {
		return nil, errors.New("input contains no scans")
	}
	return file.Scans, nil
}

func writeOutFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("create out dir: %w", err)
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func cmdRunBatch(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs := newFlagSet("runbatch", stderr)
	dbPath := fs.String("db", "", "SQLite database path")
	outDir := fs.String("out", "", "evidence output directory")
	engineName := fs.String("engine", "simulator", "OCR engine: simulator|tesseract")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	if *outDir == "" {
		return exitError, errors.New("flag --out is required")
	}
	engine, err := ocr.NewEngine(*engineName)
	if err != nil {
		return exitError, err // includes ErrTesseractUnavailable: explicit, no silent fallback
	}
	store, err := openStore(*dbPath)
	if err != nil {
		return exitError, err
	}
	defer store.Close()
	orders, err := ensureOrders(ctx, store)
	if err != nil {
		return exitError, err
	}
	manifest, err := generateManifest(orders, *outDir)
	if err != nil {
		return exitError, err
	}
	validator, err := newValidator(store)
	if err != nil {
		return exitError, err
	}
	deps := app.BatchDeps{Engine: engine, Validator: validator, Clock: ports.SystemClock{}}
	batch, err := app.RunBatch(ctx, deps, app.DemoCases(orders), manifest)
	if err != nil {
		return exitError, err
	}
	if err := writeBatchEvidence(*outDir, batch); err != nil {
		return exitError, err
	}
	fmt.Fprint(stdout, report.TextSummary(batch))
	if batch.Aggregate.Mismatched > 0 {
		return exitMismatch, fmt.Errorf("batch mismatched expectations in %d case(s)", batch.Aggregate.Mismatched)
	}
	return exitOK, nil
}

func writeBatchEvidence(outDir string, batch report.BatchReport) error {
	jsonData, err := report.JSONBytes(batch)
	if err != nil {
		return err
	}
	if _, err := report.WriteFile(outDir, "batch-report.json", jsonData); err != nil {
		return err
	}
	htmlData, err := report.HTMLBytes(batch)
	if err != nil {
		return err
	}
	if _, err := report.WriteFile(outDir, "report.html", htmlData); err != nil {
		return err
	}
	if _, err := report.WriteFile(outDir, "summary.txt", []byte(report.TextSummary(batch))); err != nil {
		return err
	}
	return nil
}
