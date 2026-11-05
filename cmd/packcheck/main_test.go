package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIVersionAndUnknownCommand(t *testing.T) {
	code, out, _ := runCLI(t, "version")
	if code != exitOK || !strings.Contains(out, "packcheck") {
		t.Fatalf("version: code=%d out=%q", code, out)
	}
	code, _, _ = runCLI(t, "inventado")
	if code != exitError {
		t.Fatalf("unknown command code=%d want %d", code, exitError)
	}
}

func TestCLIValidateExitCodes(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "test.db")
	if code, _, errs := runCLI(t, "seed", "--db", db); code != exitOK {
		t.Fatalf("seed code=%d errs=%s", code, errs)
	}
	writeScans := func(name, lot string) string {
		t.Helper()
		scan := map[string]any{
			"scans": []map[string]any{{
				"box_id": "BOX-CLI", "order_id": "OP-2026-0001", "shift": "A",
				"label": map[string]any{
					"product_text": "HEAVY DUTY FRONT BRAKE SERVICE KIT",
					"gtin":         "7501234500012",
					"lot":          lot,
					"expiry_text":  "2027-06-30",
				},
				"contents": []map[string]any{
					{"code": "PAD-01", "qty": 4},
					{"code": "ROTOR-01", "qty": 2},
					{"code": "GREASE-05G", "qty": 1},
				},
			}},
		}
		data, err := json.Marshal(scan)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	good := writeScans("good.json", "L26041A")
	code, out, _ := runCLI(t, "validate", "--db", db, "--input", good)
	if code != exitOK || !strings.Contains(out, `"verdict": "pass"`) {
		t.Fatalf("good scan: code=%d out=%s", code, out)
	}
	bad := writeScans("bad.json", "L99999X")
	code, out, _ = runCLI(t, "validate", "--db", db, "--input", bad)
	if code != exitRejected || !strings.Contains(out, "LOT_MISMATCH") {
		t.Fatalf("bad scan: code=%d out=%s (want %d + LOT_MISMATCH)", code, out, exitRejected)
	}
}

func TestCLIRunBatchProducesEvidence(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "test.db")
	out := filepath.Join(dir, "evidence")
	code, stdout, errs := runCLI(t, "runbatch", "--db", db, "--out", out)
	if code != exitOK {
		t.Fatalf("runbatch code=%d\nstdout=%s\nstderr=%s", code, stdout, errs)
	}
	for _, name := range []string{"manifest.json", "batch-report.json", "report.html", "summary.txt"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("missing evidence file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "labels", "05-blurry.png")); err != nil {
		t.Errorf("missing degraded image: %v", err)
	}
	if !strings.Contains(stdout, "Critical recall: 100.0%") {
		t.Errorf("stdout summary must show critical recall gate:\n%s", stdout)
	}
}

func TestCLITesseractMissingIsExplicitError(t *testing.T) {
	// Where tesseract exists the command works; without it, it must fail
	// naming the reason, never silently degrade to the simulator.
	dir := t.TempDir()
	code, _, errs := runCLI(t, "runbatch", "--db", filepath.Join(dir, "t.db"),
		"--out", filepath.Join(dir, "ev"), "--engine", "tesseract")
	if code != exitOK && !strings.Contains(errs, "tesseract") {
		t.Fatalf("failure must name tesseract explicitly: code=%d stderr=%s", code, errs)
	}
}
