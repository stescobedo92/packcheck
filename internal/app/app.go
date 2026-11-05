// Package app contains the use cases: order seeding, label generation with a
// manifest, scan validation, and the end-to-end demo batch. The CLI only
// translates flags into calls here.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"packcheck/internal/domain"
	"packcheck/internal/labelgen"
	"packcheck/internal/metrics"
	"packcheck/internal/ocr"
	"packcheck/internal/ports"
	"packcheck/internal/report"
	"packcheck/internal/validation"
)

// DemoOrders returns the demo production orders (different suppliers and
// fonts). GTINs are completed with a computed check digit, never hand-written
// unverified.
func DemoOrders() []domain.Order {
	return []domain.Order{
		{
			ID: "OP-2026-0001", SKU: "BRAKE-KIT-01",
			ProductName: "HEAVY DUTY FRONT BRAKE SERVICE KIT",
			GTIN:        domain.WithCheckDigit("750123450001"),
			Lot:         "L26041A",
			Expiry:      time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC),
			Contents: []domain.ContentItem{
				{Code: "PAD-01", Description: "Brake pad", Qty: 4},
				{Code: "ROTOR-01", Description: "Brake rotor", Qty: 2},
				{Code: "GREASE-05G", Description: "Grease packet 5g", Qty: 1},
			},
			Supplier: "NORTH LABELS", FontProfile: "font-a",
		},
		{
			ID: "OP-2026-0002", SKU: "AIR-FILTER-02",
			ProductName: "INDUSTRIAL REUSABLE AIR FILTER ELEMENT",
			GTIN:        domain.WithCheckDigit("750123450002"),
			Lot:         "L26052B",
			Expiry:      time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC),
			Contents: []domain.ContentItem{
				{Code: "FILTER-02", Description: "Filter element", Qty: 1},
				{Code: "SEAL-02", Description: "Rubber seal", Qty: 2},
			},
			Supplier: "SOUTH PRINT LABELS", FontProfile: "font-b",
		},
		{
			ID: "OP-2026-0003", SKU: "HOSE-KIT-03",
			ProductName: "HYDRAULIC HOSE ASSEMBLY REPAIR KIT",
			GTIN:        domain.WithCheckDigit("750123450003"),
			Lot:         "L26063C",
			Expiry:      time.Date(2027, 9, 30, 0, 0, 0, 0, time.UTC),
			Contents: []domain.ContentItem{
				{Code: "HOSE-03", Description: "Hydraulic hose", Qty: 1},
				{Code: "FITTING-03", Description: "Hose fitting", Qty: 4},
				{Code: "CLAMP-03", Description: "Hose clamp", Qty: 2},
			},
			Supplier: "NORTH LABELS", FontProfile: "font-a",
		},
		{
			ID: "OP-2026-0004", SKU: "PUMP-KIT-04",
			ProductName: "ELECTRIC WATER PUMP REPLACEMENT KIT",
			GTIN:        domain.WithCheckDigit("750123450004"),
			Lot:         "L26074D",
			Expiry:      time.Date(2028, 3, 31, 0, 0, 0, 0, time.UTC),
			Contents: []domain.ContentItem{
				{Code: "PUMP-04", Description: "Electric water pump", Qty: 1},
				{Code: "GASKET-04", Description: "Pump gasket", Qty: 1},
				{Code: "BOLT-04", Description: "Mounting bolt", Qty: 4},
			},
			Supplier: "SOUTH PRINT LABELS", FontProfile: "font-b",
		},
	}
}

// SeedDemo inserts the demo orders into the SQL repository.
func SeedDemo(ctx context.Context, repo ports.OrderRepository) ([]domain.Order, error) {
	orders := DemoOrders()
	for _, o := range orders {
		if err := repo.UpsertOrder(ctx, o); err != nil {
			return nil, fmt.Errorf("app: seed order %s: %w", o.ID, err)
		}
	}
	return orders, nil
}

// TruthOfOrder is what a correct label for that order must say.
func TruthOfOrder(o domain.Order) ports.GroundTruth {
	return ports.GroundTruth{
		ProductText: o.ProductName, GTIN: domain.OnlyDigits(o.GTIN),
		Lot: domain.NormalizeCode(o.Lot), ExpiryText: o.ExpiryDate(),
	}
}

func reportTruth(t ports.GroundTruth) report.Truth {
	return report.Truth{ProductText: t.ProductText, GTIN: t.GTIN, Lot: t.Lot, ExpiryText: t.ExpiryText}
}

// Case describes one batch case with its truth and its expected outcome,
// both declared before anything runs.
type Case struct {
	ID              string
	Description     string
	OrderID         string // order claimed by the box (the scan)
	BaseOrder       domain.Order
	BoxID           string
	Shift           string
	Profile         string
	Truth           ports.GroundTruth // what is actually printed on the label
	Contents        []domain.ContentItem
	ExpectedVerdict domain.Verdict
	ExpectedCodes   []string
	SeededError     bool
	SeededCritical  bool
}

// DemoCases builds the full specification matrix: good boxes with tolerable
// degradation, no-reads that must be rejected, and seeded errors. It expects
// orders as returned by DemoOrders (same length and order).
func DemoCases(orders []domain.Order) []Case {
	o1, o2, o3, o4 := orders[0], orders[1], orders[2], orders[3]
	t1, t2 := TruthOfOrder(o1), TruthOfOrder(o2)
	t3, t4 := TruthOfOrder(o3), TruthOfOrder(o4)
	cloneContents := func(o domain.Order) []domain.ContentItem {
		out := make([]domain.ContentItem, len(o.Contents))
		copy(out, o.Contents)
		return out
	}
	badCheckGTIN := corruptCheckDigit(t1.GTIN)

	cases := []Case{
		{ID: "01-clean-font-a", Description: "Clean label, font A, North supplier: correct box",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0001", Shift: "A", Profile: ocr.ProfileClean,
			Truth: t1, Contents: cloneContents(o1), ExpectedVerdict: domain.VerdictPass},
		{ID: "02-clean-font-b-supplier-south", Description: "Clean label, font B, South supplier: correct box",
			OrderID: o2.ID, BaseOrder: o2, BoxID: "BOX-0002", Shift: "B", Profile: ocr.ProfileSupplierB,
			Truth: t2, Contents: cloneContents(o2), ExpectedVerdict: domain.VerdictPass},
		{ID: "03-faint-print", Description: "Faint print: degraded text within CER tolerance, critical fields exact",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0003", Shift: "A", Profile: ocr.ProfileFaint,
			Truth: t1, Contents: cloneContents(o1), ExpectedVerdict: domain.VerdictPassWithWarning,
			ExpectedCodes: []string{domain.CodeTextDegraded}},
		{ID: "04-curved-surface", Description: "Label on a curved surface: tolerable degraded text",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0004", Shift: "C", Profile: ocr.ProfileCurved,
			Truth: t1, Contents: cloneContents(o1), ExpectedVerdict: domain.VerdictPassWithWarning,
			ExpectedCodes: []string{domain.CodeTextDegraded}},
		{ID: "05-blurry", Description: "Blurry label: reading quality below threshold, must be reprinted",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0005", Shift: "A", Profile: ocr.ProfileBlur,
			Truth: t1, Contents: cloneContents(o1), ExpectedVerdict: domain.VerdictReject,
			ExpectedCodes: []string{domain.CodeLabelIllegible}},
		{ID: "06-glare", Description: "Film glare covers the expiry date: rejected as illegible with unread date",
			OrderID: o2.ID, BaseOrder: o2, BoxID: "BOX-0006", Shift: "B", Profile: ocr.ProfileGlare,
			Truth: t2, Contents: cloneContents(o2), ExpectedVerdict: domain.VerdictReject,
			ExpectedCodes: []string{domain.CodeLabelIllegible, domain.CodeExpiryUnreadable}},
	}

	seeded := []Case{
		{ID: "07-wrong-product-gtin", Description: "Seeded critical: valid GTIN of a different product",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0007", Shift: "A", Profile: ocr.ProfileClean,
			Truth: withGTIN(t1, t2.GTIN), Contents: cloneContents(o1),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeGTINMismatch},
			SeededError: true, SeededCritical: true},
		{ID: "08-invalid-gtin-check-digit", Description: "Seeded critical: GTIN with invalid check digit",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0008", Shift: "B", Profile: ocr.ProfileClean,
			Truth: withGTIN(t1, badCheckGTIN), Contents: cloneContents(o1),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeGTINInvalid},
			SeededError: true, SeededCritical: true},
		{ID: "09-wrong-lot", Description: "Seeded critical: lot differs from the order",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0009", Shift: "C", Profile: ocr.ProfileClean,
			Truth: withLot(t1, "L99999X"), Contents: cloneContents(o1),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeLotMismatch},
			SeededError: true, SeededCritical: true},
		{ID: "10-wrong-expiry", Description: "Seeded critical: expiry differs from the order",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0010", Shift: "A", Profile: ocr.ProfileClean,
			Truth: withExpiry(t1, "2028-12-31"), Contents: cloneContents(o1),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeExpiryMismatch},
			SeededError: true, SeededCritical: true},
		{ID: "11-expired-product", Description: "Seeded critical: expired product",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0011", Shift: "B", Profile: ocr.ProfileClean,
			Truth: withExpiry(t1, "2020-01-15"), Contents: cloneContents(o1),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeExpiryExpired},
			SeededError: true, SeededCritical: true},
		{ID: "12-unreadable-expiry", Description: "Seeded critical: printed expiry is unreadable",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0012", Shift: "C", Profile: ocr.ProfileClean,
			Truth: withExpiry(t1, "##/##/####"), Contents: cloneContents(o1),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeExpiryUnreadable},
			SeededError: true, SeededCritical: true},
		{ID: "13-wrong-product-label", Description: "Seeded critical: label of a different product",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0013", Shift: "A", Profile: ocr.ProfileClean,
			Truth: withText(t1, "INDUSTRIAL REUSABLE AIR FILTER ELEMENT"), Contents: cloneContents(o1),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeTextMismatch},
			SeededError: true, SeededCritical: true},
		{ID: "14-missing-kit-component", Description: "Seeded critical: kit component missing",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0014", Shift: "B", Profile: ocr.ProfileClean,
			Truth: t1, Contents: dropCode(cloneContents(o1), "ROTOR-01"),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeContentMissing},
			SeededError: true, SeededCritical: true},
		{ID: "15-short-kit-quantity", Description: "Seeded critical: component quantity short",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0015", Shift: "C", Profile: ocr.ProfileClean,
			Truth: t1, Contents: setQty(cloneContents(o1), "PAD-01", 3),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeContentShort},
			SeededError: true, SeededCritical: true},
		{ID: "16-extra-component", Description: "Seeded major: component not part of the kit",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0016", Shift: "A", Profile: ocr.ProfileClean,
			Truth: t1, Contents: append(cloneContents(o1), domain.ContentItem{Code: "SCREW-99", Qty: 2}),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeContentExtra},
			SeededError: true, SeededCritical: false},
		{ID: "17-unknown-order", Description: "Seeded critical: the box claims an order that is not in SQL",
			OrderID: "OP-9999-9999", BaseOrder: o1, BoxID: "BOX-0017", Shift: "B", Profile: ocr.ProfileClean,
			Truth: t1, Contents: cloneContents(o1),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeOrderNotFound},
			SeededError: true, SeededCritical: true},
	}
	extended := []Case{
		{ID: "18-clean-hose-kit", Description: "Clean label, hydraulic hose kit order: correct box",
			OrderID: o3.ID, BaseOrder: o3, BoxID: "BOX-0018", Shift: "B", Profile: ocr.ProfileClean,
			Truth: t3, Contents: cloneContents(o3), ExpectedVerdict: domain.VerdictPass},
		{ID: "19-clean-pump-kit", Description: "Clean label font B, electric pump kit order: correct box",
			OrderID: o4.ID, BaseOrder: o4, BoxID: "BOX-0019", Shift: "C", Profile: ocr.ProfileClean,
			Truth: t4, Contents: cloneContents(o4), ExpectedVerdict: domain.VerdictPass},
		{ID: "20-faint-hose-kit", Description: "Faint print on the hose kit: tolerated text degradation",
			OrderID: o3.ID, BaseOrder: o3, BoxID: "BOX-0020", Shift: "A", Profile: ocr.ProfileFaint,
			Truth: t3, Contents: cloneContents(o3), ExpectedVerdict: domain.VerdictPassWithWarning,
			ExpectedCodes: []string{domain.CodeTextDegraded}},
		{ID: "21-curved-pump-kit", Description: "Curved surface on the pump kit: tolerated text degradation",
			OrderID: o4.ID, BaseOrder: o4, BoxID: "BOX-0021", Shift: "B", Profile: ocr.ProfileCurved,
			Truth: t4, Contents: cloneContents(o4), ExpectedVerdict: domain.VerdictPassWithWarning,
			ExpectedCodes: []string{domain.CodeTextDegraded}},
		{ID: "22-blurry-pump-kit", Description: "Blurry pump kit label: rejected as illegible",
			OrderID: o4.ID, BaseOrder: o4, BoxID: "BOX-0022", Shift: "C", Profile: ocr.ProfileBlur,
			Truth: t4, Contents: cloneContents(o4), ExpectedVerdict: domain.VerdictReject,
			ExpectedCodes: []string{domain.CodeLabelIllegible}},
		{ID: "23-glare-brake-kit", Description: "Glare over the brake kit expiry: rejected, date unread",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0023", Shift: "A", Profile: ocr.ProfileGlare,
			Truth: t1, Contents: cloneContents(o1), ExpectedVerdict: domain.VerdictReject,
			ExpectedCodes: []string{domain.CodeLabelIllegible, domain.CodeExpiryUnreadable}},
		{ID: "24-month-year-expiry", Description: "Expiry printed as month/year (06/2027): accepted format, correct box",
			OrderID: o1.ID, BaseOrder: o1, BoxID: "BOX-0024", Shift: "B", Profile: ocr.ProfileClean,
			Truth: withExpiry(t1, "06/2027"), Contents: cloneContents(o1), ExpectedVerdict: domain.VerdictPass},
		{ID: "25-slash-expiry", Description: "Expiry printed as DD/MM/YYYY (31/01/2027): accepted format, correct box",
			OrderID: o2.ID, BaseOrder: o2, BoxID: "BOX-0025", Shift: "C", Profile: ocr.ProfileClean,
			Truth: withExpiry(t2, "31/01/2027"), Contents: cloneContents(o2), ExpectedVerdict: domain.VerdictPass},
		{ID: "26-invalid-lot-format", Description: "Seeded critical: lot printed with an invalid format",
			OrderID: o3.ID, BaseOrder: o3, BoxID: "BOX-0026", Shift: "A", Profile: ocr.ProfileClean,
			Truth: withLot(t3, "##BAD##"), Contents: cloneContents(o3),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeLotInvalid},
			SeededError: true, SeededCritical: true},
		{ID: "27-empty-product-text", Description: "Seeded critical: product text missing from the label",
			OrderID: o4.ID, BaseOrder: o4, BoxID: "BOX-0027", Shift: "B", Profile: ocr.ProfileClean,
			Truth: withText(t4, ""), Contents: cloneContents(o4),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeTextMissing},
			SeededError: true, SeededCritical: true},
		{ID: "28-over-quantity", Description: "Seeded major: more seals in the box than the order requires",
			OrderID: o2.ID, BaseOrder: o2, BoxID: "BOX-0028", Shift: "C", Profile: ocr.ProfileClean,
			Truth: t2, Contents: setQty(cloneContents(o2), "SEAL-02", 5),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeContentOver},
			SeededError: true, SeededCritical: false},
		{ID: "29-wrong-lot-pump-kit", Description: "Seeded critical: pump kit lot differs from the order",
			OrderID: o4.ID, BaseOrder: o4, BoxID: "BOX-0029", Shift: "A", Profile: ocr.ProfileClean,
			Truth: withLot(t4, "L99999X"), Contents: cloneContents(o4),
			ExpectedVerdict: domain.VerdictReject, ExpectedCodes: []string{domain.CodeLotMismatch},
			SeededError: true, SeededCritical: true},
	}
	return append(append(cases, seeded...), extended...)
}

func withGTIN(t ports.GroundTruth, gtin string) ports.GroundTruth { t.GTIN = gtin; return t }
func withLot(t ports.GroundTruth, lot string) ports.GroundTruth   { t.Lot = lot; return t }
func withExpiry(t ports.GroundTruth, e string) ports.GroundTruth  { t.ExpiryText = e; return t }
func withText(t ports.GroundTruth, s string) ports.GroundTruth    { t.ProductText = s; return t }

// corruptCheckDigit changes the last digit, guaranteeing an invalid check digit.
func corruptCheckDigit(gtin string) string {
	d := []byte(domain.OnlyDigits(gtin))
	if len(d) != 13 {
		return gtin
	}
	d[12] = '0' + (d[12]-'0'+1)%10
	return string(d)
}

func dropCode(items []domain.ContentItem, code string) []domain.ContentItem {
	out := make([]domain.ContentItem, 0, len(items))
	for _, it := range items {
		if domain.NormalizeCode(it.Code) != domain.NormalizeCode(code) {
			out = append(out, it)
		}
	}
	return out
}

func setQty(items []domain.ContentItem, code string, qty int) []domain.ContentItem {
	out := make([]domain.ContentItem, len(items))
	copy(out, items)
	for i := range out {
		if domain.NormalizeCode(out[i].Code) == domain.NormalizeCode(code) {
			out[i].Qty = qty
		}
	}
	return out
}

// labelContentOf translates a case into what is physically printed.
func labelContentOf(c Case) labelgen.Content {
	font := c.BaseOrder.FontProfile
	if c.Profile == ocr.ProfileFontB {
		font = "font-b"
	}
	return labelgen.Content{
		Supplier: c.BaseOrder.Supplier, ProductName: c.Truth.ProductText,
		SKU: c.BaseOrder.SKU, GTIN: c.Truth.GTIN, Lot: c.Truth.Lot,
		ExpiryText: c.Truth.ExpiryText, OrderID: c.OrderID, FontProfile: font,
	}
}

// GenerateLabels renders the case images and builds the manifest.
func GenerateLabels(cases []Case, labelsDir string, now time.Time) (report.Manifest, []string, error) {
	manifest := report.Manifest{GeneratedAt: now.UTC().Format(time.RFC3339), Cases: []report.ManifestCase{}}
	imageNames := make([]string, len(cases))
	for i, c := range cases {
		img, err := labelgen.Render(labelContentOf(c), c.Profile)
		if err != nil {
			return report.Manifest{}, nil, fmt.Errorf("app: render case %s: %w", c.ID, err)
		}
		name := c.ID + ".png"
		path, err := labelgen.WriteImage(labelsDir, name, img)
		if err != nil {
			return report.Manifest{}, nil, fmt.Errorf("app: write image case %s: %w", c.ID, err)
		}
		sum, err := SHA256File(path)
		if err != nil {
			return report.Manifest{}, nil, err
		}
		imageNames[i] = name
		manifest.Cases = append(manifest.Cases, report.ManifestCase{
			CaseID: c.ID, Description: c.Description, OrderID: c.OrderID, BoxID: c.BoxID,
			Shift: c.Shift, Profile: c.Profile, ImageFile: name, ImageSHA256: sum,
			Truth: reportTruth(c.Truth), ExpectedVerdict: c.ExpectedVerdict,
			ExpectedCodes: append([]string{}, c.ExpectedCodes...),
			SeededError:   c.SeededError, SeededCritical: c.SeededCritical,
		})
	}
	return manifest, imageNames, nil
}

// SHA256File computes a file's hex fingerprint (streaming, never fully loaded).
func SHA256File(path string) (string, error) {
	// #nosec G304 -- path comes from WriteImage inside the evidence directory
	// chosen by the operator (confined with os.OpenRoot), never from remote
	// input; this function only computes its fingerprint.
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("app: open for hash: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("app: hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// BatchDeps groups the RunBatch dependencies (no globals).
type BatchDeps struct {
	Engine    ports.OCREngine
	Validator *validation.BoxValidator
	Clock     ports.Clock
}

// RunBatch runs the full flow per case: generated image, OCR reading, SQL
// cross-check and verdict, aggregating reading and gate metrics.
func RunBatch(ctx context.Context, deps BatchDeps, cases []Case, manifest report.Manifest) (report.BatchReport, error) {
	if deps.Engine == nil || deps.Validator == nil || deps.Clock == nil {
		return report.BatchReport{}, fmt.Errorf("app: engine, validator and clock are required")
	}
	stats := map[domain.Field]*metrics.FieldStat{
		domain.FieldText: {}, domain.FieldGTIN: {}, domain.FieldLot: {}, domain.FieldExpiry: {},
	}
	caseReports := make([]report.CaseReport, 0, len(cases))
	durations := make([]time.Duration, 0, len(cases))

	for i, c := range cases {
		imageName, imageSum := "", ""
		if i < len(manifest.Cases) {
			imageName = manifest.Cases[i].ImageFile
			imageSum = manifest.Cases[i].ImageSHA256
		}
		reading, err := deps.Engine.Read(ctx, ports.OCRRequest{
			BoxID: c.BoxID,
			Image: ports.LabelImage{Path: filepath.Join("labels", imageName), Profile: c.Profile},
			Truth: c.Truth,
		})
		if err != nil {
			return report.BatchReport{}, fmt.Errorf("app: ocr case %s: %w", c.ID, err)
		}
		observeFields(stats, c.Truth, reading)

		scan := domain.BoxScan{
			BoxID: c.BoxID, OrderID: c.OrderID, Shift: c.Shift,
			Label: reading, Contents: c.Contents,
		}
		result, err := deps.Validator.ValidateBox(ctx, scan)
		if err != nil {
			return report.BatchReport{}, fmt.Errorf("app: validate case %s: %w", c.ID, err)
		}
		durations = append(durations, time.Duration(result.CycleNanos))
		caseReports = append(caseReports, report.CaseReport{
			CaseID: c.ID, Description: c.Description, BoxID: c.BoxID, OrderID: c.OrderID,
			Shift: c.Shift, Profile: c.Profile, ImageFile: imageName, ImageSHA256: imageSum,
			Truth: reportTruth(c.Truth), Reading: reading,
			ExpectedVerdict: c.ExpectedVerdict, GotVerdict: result.Verdict,
			ExpectedCodes:   append([]string{}, c.ExpectedCodes...),
			Findings:        result.Findings,
			MatchesExpected: matchesExpected(result, c),
			SeededError:     c.SeededError, SeededCritical: c.SeededCritical,
			CycleNanos: result.CycleNanos,
		})
	}

	fields := []metrics.FieldReport{
		stats[domain.FieldText].Report(string(domain.FieldText)),
		stats[domain.FieldGTIN].Report(string(domain.FieldGTIN)),
		stats[domain.FieldLot].Report(string(domain.FieldLot)),
		stats[domain.FieldExpiry].Report(string(domain.FieldExpiry)),
	}
	return report.BatchReport{
		GeneratedAt: deps.Clock.Now().UTC().Format(time.RFC3339),
		Engine:      deps.Engine.Name(),
		Fields:      fields,
		Aggregate:   report.BuildAggregate(caseReports, durations),
		Cases:       caseReports,
	}, nil
}

// observeFields measures OCR fidelity: reading against what is printed (not
// against the order; cross-checking the order is the validators' job).
func observeFields(stats map[domain.Field]*metrics.FieldStat, truth ports.GroundTruth, r domain.LabelReading) {
	stats[domain.FieldText].Observe(domain.NormalizeText(truth.ProductText), domain.NormalizeText(r.ProductText))
	stats[domain.FieldGTIN].Observe(domain.OnlyDigits(truth.GTIN), domain.OnlyDigits(r.GTIN))
	stats[domain.FieldLot].Observe(domain.NormalizeCode(truth.Lot), domain.NormalizeCode(r.Lot))
	stats[domain.FieldExpiry].Observe(
		strings.TrimSpace(strings.ToUpper(truth.ExpiryText)),
		strings.TrimSpace(strings.ToUpper(r.ExpiryText)),
	)
}

// matchesExpected requires an identical verdict and every expected code present.
func matchesExpected(result domain.Result, c Case) bool {
	if result.Verdict != c.ExpectedVerdict {
		return false
	}
	for _, code := range c.ExpectedCodes {
		if !result.HasCode(code) {
			return false
		}
	}
	return true
}
