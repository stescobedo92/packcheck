package validation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"packcheck/internal/domain"
	"packcheck/internal/ports"
)

type memRepo struct{ orders map[string]domain.Order }

func (m memRepo) UpsertOrder(_ context.Context, o domain.Order) error {
	m.orders[o.ID] = o
	return nil
}
func (m memRepo) GetOrder(_ context.Context, id string) (domain.Order, error) {
	o, ok := m.orders[id]
	if !ok {
		return domain.Order{}, fmt.Errorf("%w: %s", domain.ErrOrderNotFound, id)
	}
	return o, nil
}
func (m memRepo) ListOrders(_ context.Context) ([]domain.Order, error) {
	out := make([]domain.Order, 0, len(m.orders))
	for _, o := range m.orders {
		out = append(out, o)
	}
	return out, nil
}

var fixedNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func testOrder() domain.Order {
	return domain.Order{
		ID: "OP-1", SKU: "S1", ProductName: "HEAVY DUTY FRONT BRAKE SERVICE KIT",
		GTIN:   domain.WithCheckDigit("750123450001"),
		Lot:    "L26041A",
		Expiry: time.Date(2027, 6, 30, 0, 0, 0, 0, time.UTC),
		Contents: []domain.ContentItem{
			{Code: "PAD-01", Qty: 4}, {Code: "ROTOR-01", Qty: 2},
		},
	}
}

func goodScan(o domain.Order) domain.BoxScan {
	contents := make([]domain.ContentItem, len(o.Contents))
	copy(contents, o.Contents)
	return domain.BoxScan{
		BoxID: "BOX-1", OrderID: o.ID, Shift: "A",
		Label:    domain.ManualReading(o.ProductName, o.GTIN, o.Lot, o.ExpiryDate()),
		Contents: contents,
	}
}

func newTestValidator(t *testing.T, repo ports.OrderRepository) *BoxValidator {
	t.Helper()
	v, err := NewBoxValidator(repo, ports.FixedClock{T: fixedNow}, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestValidateBoxGoodPasses(t *testing.T) {
	o := testOrder()
	repo := memRepo{orders: map[string]domain.Order{o.ID: o}}
	v := newTestValidator(t, repo)
	res, err := v.ValidateBox(context.Background(), goodScan(o))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != domain.VerdictPass || len(res.Findings) != 0 {
		t.Fatalf("verdict=%v findings=%+v", res.Verdict, res.Findings)
	}
}

func TestValidateBoxEachSeededErrorIsRejected(t *testing.T) {
	o := testOrder()
	repo := memRepo{orders: map[string]domain.Order{o.ID: o}}
	v := newTestValidator(t, repo)
	otherGTIN := domain.WithCheckDigit("750123450099")

	cases := []struct {
		name   string
		mutate func(*domain.BoxScan)
		code   string
	}{
		{"wrong gtin", func(s *domain.BoxScan) { s.Label.GTIN = otherGTIN }, domain.CodeGTINMismatch},
		{"invalid gtin", func(s *domain.BoxScan) { s.Label.GTIN = "7501234500013" }, domain.CodeGTINInvalid},
		{"wrong lot", func(s *domain.BoxScan) { s.Label.Lot = "L99999X" }, domain.CodeLotMismatch},
		{"lot format", func(s *domain.BoxScan) { s.Label.Lot = "##" }, domain.CodeLotInvalid},
		{"wrong expiry", func(s *domain.BoxScan) { s.Label.ExpiryText = "2028-01-01" }, domain.CodeExpiryMismatch},
		{"expired", func(s *domain.BoxScan) { s.Label.ExpiryText = "2020-01-01" }, domain.CodeExpiryExpired},
		{"unreadable expiry", func(s *domain.BoxScan) { s.Label.ExpiryText = "##/##/####" }, domain.CodeExpiryUnreadable},
		{"wrong text", func(s *domain.BoxScan) { s.Label.ProductText = "AIR FILTER ELEMENT" }, domain.CodeTextMismatch},
		{"empty text", func(s *domain.BoxScan) { s.Label.ProductText = "" }, domain.CodeTextMissing},
		{"missing kit item", func(s *domain.BoxScan) { s.Contents = s.Contents[:1] }, domain.CodeContentMissing},
		{"short kit", func(s *domain.BoxScan) { s.Contents[0].Qty = 1 }, domain.CodeContentShort},
		{"extra", func(s *domain.BoxScan) {
			s.Contents = append(s.Contents, domain.ContentItem{Code: "X-1", Qty: 1})
		}, domain.CodeContentExtra},
		{"over quantity", func(s *domain.BoxScan) { s.Contents[0].Qty = 99 }, domain.CodeContentOver},
		{"illegible", func(s *domain.BoxScan) {
			s.Label.QualityKnown, s.Label.Confidence, s.Label.Legibility = true, 0.5, 0.9
		}, domain.CodeLabelIllegible},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scan := goodScan(o)
			tc.mutate(&scan)
			res, err := v.ValidateBox(context.Background(), scan)
			if err != nil {
				t.Fatal(err)
			}
			if res.Verdict != domain.VerdictReject {
				t.Fatalf("verdict=%v want reject; findings=%+v", res.Verdict, res.Findings)
			}
			if !res.HasCode(tc.code) {
				t.Fatalf("missing code %s in %+v", tc.code, res.Findings)
			}
		})
	}
}

func TestValidateBoxOrderNotFoundIsCriticalRejectNotError(t *testing.T) {
	repo := memRepo{orders: map[string]domain.Order{}}
	v := newTestValidator(t, repo)
	scan := goodScan(testOrder())
	res, err := v.ValidateBox(context.Background(), scan)
	if err != nil {
		t.Fatalf("order-not-found must be a reject, not infra error: %v", err)
	}
	if res.Verdict != domain.VerdictReject || !res.HasCode(domain.CodeOrderNotFound) {
		t.Fatalf("res=%+v", res)
	}
}

func TestValidateBoxDegradedTextPassesWithWarnings(t *testing.T) {
	o := testOrder()
	repo := memRepo{orders: map[string]domain.Order{o.ID: o}}
	v := newTestValidator(t, repo)
	scan := goodScan(o)
	scan.Label.ProductText = "HFAVY DUTY FRONT BRAKE SERVICE KIT"
	res, err := v.ValidateBox(context.Background(), scan)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != domain.VerdictPassWithWarning || !res.HasCode(domain.CodeTextDegraded) {
		t.Fatalf("res=%+v", res)
	}
}

func TestExpiryTodayIsNotExpired(t *testing.T) {
	o := testOrder()
	o.Expiry = domain.TruncateToDay(fixedNow)
	repo := memRepo{orders: map[string]domain.Order{o.ID: o}}
	v := newTestValidator(t, repo)
	res, err := v.ValidateBox(context.Background(), goodScan(o))
	if err != nil {
		t.Fatal(err)
	}
	if res.HasCode(domain.CodeExpiryExpired) {
		t.Fatalf("expiry == today must not be expired: %+v", res.Findings)
	}
}

func TestNewBoxValidatorRejectsBadConfig(t *testing.T) {
	repo := memRepo{orders: map[string]domain.Order{}}
	if _, err := NewBoxValidator(repo, ports.FixedClock{}, Config{}); err == nil {
		t.Fatal("zero thresholds must be rejected at construction")
	}
	if _, err := NewBoxValidator(nil, ports.FixedClock{}, DefaultConfig()); err == nil {
		t.Fatal("nil repo must be rejected")
	}
}

func BenchmarkValidateBox(b *testing.B) {
	o := testOrder()
	repo := memRepo{orders: map[string]domain.Order{o.ID: o}}
	v, err := NewBoxValidator(repo, ports.FixedClock{T: fixedNow}, DefaultConfig())
	if err != nil {
		b.Fatal(err)
	}
	scan := goodScan(o)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.ValidateBox(ctx, scan); err != nil {
			b.Fatal(err)
		}
	}
}
