package labelgen

import (
	"bytes"
	"errors"
	"image/png"
	"testing"

	"packcheck/internal/domain"
	"packcheck/internal/ocr"
)

func sampleContent() Content {
	return Content{
		Supplier: "NORTH LABELS", ProductName: "HEAVY DUTY FRONT BRAKE SERVICE KIT",
		SKU: "BRAKE-KIT-01", GTIN: "7501234500012", Lot: "L26041A",
		ExpiryText: "2027-06-30", OrderID: "OP-2026-0001", FontProfile: "font-a",
	}
}

func TestGlyphCoverageForLabelAlphabet(t *testing.T) {
	for _, r := range "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-/:.# " {
		if !HasGlyph(r) {
			t.Errorf("missing glyph for %q", r)
		}
	}
}

func diffCount(a, b [][4]uint8) int {
	n := 0
	for i := range a {
		if a[i] != b[i] {
			n++
		}
	}
	return n
}

func pixelsOf(t *testing.T, profile string, c Content) [][4]uint8 {
	t.Helper()
	img, err := Render(c, profile)
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != Width || img.Bounds().Dy() != Height {
		t.Fatalf("size = %v", img.Bounds())
	}
	out := make([][4]uint8, 0, len(img.Pix)/4)
	for i := 0; i < len(img.Pix); i += 4 {
		out = append(out, [4]uint8{img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3]})
	}
	return out
}

func meanLuminance(px [][4]uint8) float64 {
	var sum float64
	for _, p := range px {
		sum += float64(p[0]) + float64(p[1]) + float64(p[2])
	}
	return sum / float64(len(px)*3)
}

func TestRenderEveryProfileAndDegradationsChangePixels(t *testing.T) {
	c := sampleContent()
	clean := pixelsOf(t, ocr.ProfileClean, c)
	for _, p := range []string{ocr.ProfileFaint, ocr.ProfileBlur, ocr.ProfileGlare, ocr.ProfileCurved} {
		px := pixelsOf(t, p, c)
		if d := diffCount(clean, px); d < 1000 {
			t.Errorf("profile %q barely changes the image: %d differing pixels", p, d)
		}
	}
	if got, want := meanLuminance(pixelsOf(t, ocr.ProfileFaint, c)), meanLuminance(clean); got <= want {
		t.Errorf("faint must be lighter: faint=%.2f clean=%.2f", got, want)
	}
	// Another font/supplier: same data, different image.
	other := c
	other.FontProfile, other.Supplier = "font-b", "SOUTH PRINT LABELS"
	if d := diffCount(clean, pixelsOf(t, ocr.ProfileSupplierB, other)); d < 1000 {
		t.Errorf("font-b/supplier-b image should differ visibly: %d px", d)
	}
	if _, err := Render(c, "infrarrojo"); err == nil {
		t.Fatal("unknown profile must error")
	}
}

func TestRenderIsDeterministicAndPNGDecodable(t *testing.T) {
	c := sampleContent()
	a, err := Render(c, ocr.ProfileBlur)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(c, ocr.ProfileBlur)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Pix, b.Pix) {
		t.Fatal("same input must render byte-identical pixels (reproducible evidence)")
	}
	var buf bytes.Buffer
	if err := EncodePNG(&buf, a); err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(&buf)
	if err != nil {
		t.Fatalf("generated PNG must decode: %v", err)
	}
	if img.Bounds().Dx() != Width || img.Bounds().Dy() != Height {
		t.Fatalf("decoded size %v", img.Bounds())
	}
}

func TestWrapKeepsFullProductNameWithinThreeLines(t *testing.T) {
	for _, name := range []string{
		"HEAVY DUTY FRONT BRAKE SERVICE KIT",
		"INDUSTRIAL REUSABLE AIR FILTER ELEMENT",
		"HYDRAULIC HOSE ASSEMBLY REPAIR KIT",
		"ELECTRIC WATER PUMP REPLACEMENT KIT",
	} {
		lines := wrap(name, 28)
		if len(lines) > 3 {
			t.Errorf("%q needs %d lines > 3 printed lines (name would be cut)", name, len(lines))
		}
		joined := ""
		for i, ln := range lines {
			if i > 0 {
				joined += " "
			}
			joined += ln
		}
		if joined != name {
			t.Errorf("wrap lost words: got %q want %q", joined, name)
		}
	}
}

func TestWriteImageRejectsTraversal(t *testing.T) {
	img, err := Render(sampleContent(), ocr.ProfileClean)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteImage(t.TempDir(), "..\\evil.png", img); err == nil {
		t.Fatal("traversal name must be rejected")
	} else if !errors.Is(err, domain.ErrUnsafePath) {
		t.Fatalf("err=%v want ErrUnsafePath", err)
	}
}
