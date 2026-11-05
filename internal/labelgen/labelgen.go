// Package labelgen generates box-label PNG images with the standard library,
// including font/supplier profiles and reproducible physical degradations
// (faint, blurry, glare, curved). The images are the evidence a human opens
// and compares against the manifest and the report.
package labelgen

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"packcheck/internal/domain"
	"packcheck/internal/ocr"
)

// Fixed dimensions of the generated label.
const (
	Width  = 720
	Height = 420
)

var (
	white     = color.RGBA{R: 255, G: 255, B: 255, A: 255}
	black     = color.RGBA{R: 20, G: 20, B: 20, A: 255}
	bandBlue  = color.RGBA{R: 24, G: 54, B: 120, A: 255}
	bandGreen = color.RGBA{R: 20, G: 96, B: 60, A: 255}
	gray      = color.RGBA{R: 90, G: 90, B: 90, A: 255}
)

// Content is everything printed on the physical label.
type Content struct {
	Supplier    string // e.g. NORTH LABELS / SOUTH PRINT LABELS
	ProductName string
	SKU         string
	GTIN        string
	Lot         string
	ExpiryText  string // exactly as printed (YYYY-MM-DD)
	OrderID     string
	FontProfile string // font-a | font-b
}

// textStyle captures the typographic differences between profiles: font B
// prints with a bold stroke and extra tracking, as another supplier would.
type textStyle struct {
	bold     bool
	tracking int
	band     color.RGBA
}

func styleFor(fontProfile, supplier string) textStyle {
	st := textStyle{band: bandBlue}
	if strings.EqualFold(fontProfile, "font-b") {
		st.bold = true
		st.tracking = 2
	}
	if strings.Contains(strings.ToUpper(supplier), "SOUTH") {
		st.band = bandGreen
	}
	return st
}

// Render draws the clean label and then applies the profile degradation.
// Valid profiles are those in internal/ocr (clean, faint, curved, blur,
// glare, font-b, supplier-b). font-b/supplier-b do not degrade pixels: their
// difference already comes through Content (font and band).
func Render(c Content, profile string) (*image.RGBA, error) {
	if _, ok := ocr.SpecFor(profile); !ok {
		return nil, fmt.Errorf("labelgen: unknown profile %q", profile)
	}
	img := renderClean(c)
	out, err := applyDegradation(img, strings.ToLower(strings.TrimSpace(profile)))
	if err != nil {
		return nil, err
	}
	return out, nil
}

func renderClean(c Content) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	draw.Draw(img, img.Bounds(), &image.Uniform{white}, image.Point{}, draw.Src)
	st := styleFor(c.FontProfile, c.Supplier)

	// Border and supplier band.
	drawRect(img, 8, 8, Width-9, Height-9, black, 3)
	fillRect(img, 9, 9, Width-10, 62, st.band)
	drawText(img, 28, 22, firstNonEmpty(c.Supplier, "SUPPLIER"), 3, st, white)
	drawText(img, 28, 72, "VERIFY BEFORE SEALING", 2, st, gray)

	// Product: up to 3 lines, wrapped on words. The full name must always fit:
	// a clipped label is not valid evidence. Names longer than 3 lines are
	// clipped visibly, so production label names must stay short (see README).
	lines := wrap(printable(c.ProductName), 28)
	y := 100
	for i, ln := range lines {
		if i >= 3 {
			break
		}
		drawText(img, 28, y, ln, 3, st, black)
		y += 34
	}
	drawText(img, 28, 208, "SKU: "+c.SKU, 2, st, gray)

	// Human-readable GTIN plus deterministic decorative bars (not a decodable
	// EAN: the code truth is the printed digits, as the specification states).
	gtin := domain.OnlyDigits(c.GTIN)
	drawText(img, 28, 238, "GTIN: "+gtin, 3, st, black)
	drawBars(img, 28, 282, gtin)

	// Lot, expiry, and order.
	drawText(img, 28, 318, "LOT: "+printable(c.Lot), 3, st, black)
	drawText(img, 380, 318, "EXP: "+printable(c.ExpiryText), 3, st, black)
	drawText(img, 28, 366, "ORDER: "+printable(c.OrderID), 2, st, gray)
	return img
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return printable(a)
	}
	return b
}

// wrap breaks text on words at a maximum of characters per line.
func wrap(text string, max int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	lines := []string{}
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > max {
			lines = append(lines, cur)
			cur = w
		} else {
			cur += " " + w
		}
	}
	return append(lines, cur)
}

// drawBars draws vertical bars whose pattern derives deterministically from
// the digits: visual barcode evidence, without promising decodability.
func drawBars(img *image.RGBA, x, y int, digits string) {
	const barH = 30
	cx := x
	for _, r := range digits {
		d := int(r - '0')
		// 4 modules per digit: widths 1-3 derived from the digit value.
		for m := 0; m < 4; m++ {
			w := 1 + ((d + m*2) % 3)
			if (d+m)%2 == 0 {
				fillRect(img, cx, y, cx+w, y+barH, black)
			}
			cx += w + 1
		}
		cx += 2
	}
}

// drawText prints scaled bitmap text. bold widens the horizontal stroke.
func drawText(img *image.RGBA, x, y int, text string, scale int, st textStyle, ink color.RGBA) {
	text = printable(text)
	cx := x
	step := (glyphW * scale) + scale + st.tracking
	for _, r := range text {
		g, ok := glyphs[r]
		if !ok {
			g = glyphs[' ']
		}
		for row := 0; row < glyphH; row++ {
			for col := 0; col < glyphW; col++ {
				if g[row]&(1<<uint(4-col)) == 0 {
					continue
				}
				px := cx + col*scale
				py := y + row*scale
				w := scale
				if st.bold {
					w = scale + 1
				}
				fillRect(img, px, py, px+w-1, py+scale-1, ink)
			}
		}
		cx += step
	}
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	b := img.Bounds()
	for y := y0; y <= y1; y++ {
		if y < b.Min.Y || y >= b.Max.Y {
			continue
		}
		for x := x0; x <= x1; x++ {
			if x < b.Min.X || x >= b.Max.X {
				continue
			}
			img.SetRGBA(x, y, c)
		}
	}
}

func drawRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA, thick int) {
	for t := 0; t < thick; t++ {
		fillRect(img, x0, y0+t, x1, y0+t, c)
		fillRect(img, x0, y1-t, x1, y1-t, c)
		fillRect(img, x0+t, y0, x0+t, y1, c)
		fillRect(img, x1-t, y0, x1-t, y1, c)
	}
}

// --- Reproducible physical degradations ---

func applyDegradation(src *image.RGBA, profile string) (*image.RGBA, error) {
	switch profile {
	case "", ocr.ProfileClean, ocr.ProfileFontB, ocr.ProfileSupplierB:
		return clone(src), nil
	case ocr.ProfileFaint:
		return fade(src, 0.55), nil
	case ocr.ProfileBlur:
		return boxBlur(boxBlur(src, 2), 2), nil
	case ocr.ProfileGlare:
		return glare(src), nil
	case ocr.ProfileCurved:
		return warpCurve(src, 14), nil
	default:
		return nil, fmt.Errorf("labelgen: unknown profile %q", profile)
	}
}

func clone(src *image.RGBA) *image.RGBA {
	out := image.NewRGBA(src.Bounds())
	copy(out.Pix, src.Pix)
	return out
}

// fade blends every pixel toward white: low-ink printing.
func fade(src *image.RGBA, towardWhite float64) *image.RGBA {
	out := clone(src)
	for i := 0; i < len(out.Pix); i += 4 {
		for ch := 0; ch < 3; ch++ {
			v := float64(out.Pix[i+ch])
			out.Pix[i+ch] = uint8(v + (255-v)*towardWhite)
		}
	}
	return out
}

// boxBlur applies a separable horizontal+vertical blur of radius r.
func boxBlur(src *image.RGBA, r int) *image.RGBA {
	if r <= 0 {
		return clone(src)
	}
	tmp := blurPass(src, r, true)
	return blurPass(tmp, r, false)
}

func blurPass(src *image.RGBA, r int, horizontal bool) *image.RGBA {
	b := src.Bounds()
	out := image.NewRGBA(b)
	win := 2*r + 1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var sr, sg, sb int
			for k := -r; k <= r; k++ {
				var px, py int
				if horizontal {
					px, py = clampInt(x+k, b.Min.X, b.Max.X-1), y
				} else {
					px, py = x, clampInt(y+k, b.Min.Y, b.Max.Y-1)
				}
				c := src.RGBAAt(px, py)
				sr += int(c.R)
				sg += int(c.G)
				sb += int(c.B)
			}
			out.SetRGBA(x, y, color.RGBA{
				R: avgByte(sr, win), G: avgByte(sg, win), B: avgByte(sb, win), A: 255,
			})
		}
	}
	return out
}

// glare simulates film reflection: a bright ellipse over the lot/expiry
// zone that saturates pixels toward white.
func glare(src *image.RGBA) *image.RGBA {
	out := clone(src)
	b := src.Bounds()
	cx, cy := float64(b.Max.X)*0.66, float64(b.Max.Y)*0.74
	rx, ry := float64(b.Max.X)*0.34, float64(b.Max.Y)*0.20
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			dx := (float64(x) - cx) / rx
			dy := (float64(y) - cy) / ry
			d2 := dx*dx + dy*dy
			if d2 >= 1 {
				continue
			}
			f := (1 - d2) * 0.96 // center is almost pure white
			c := out.RGBAAt(x, y)
			out.SetRGBA(x, y, color.RGBA{
				R: mix(c.R, f), G: mix(c.G, f), B: mix(c.B, f), A: 255,
			})
		}
	}
	return out
}

func mix(v uint8, towardWhite float64) uint8 {
	f := float64(v) + (255-float64(v))*towardWhite
	if f > 255 {
		f = 255
	}
	return uint8(f)
}

// warpCurve shifts each column vertically with a sine wave: a label stuck
// on a curved surface seen from the front.
func warpCurve(src *image.RGBA, amplitude float64) *image.RGBA {
	b := src.Bounds()
	out := image.NewRGBA(b)
	draw.Draw(out, b, &image.Uniform{white}, image.Point{}, draw.Src)
	w := float64(b.Dx())
	for x := b.Min.X; x < b.Max.X; x++ {
		dy := int(math.Round(amplitude * math.Sin(2*math.Pi*float64(x-b.Min.X)/w)))
		for y := b.Min.Y; y < b.Max.Y; y++ {
			sy := y - dy
			if sy < b.Min.Y || sy >= b.Max.Y {
				continue
			}
			out.SetRGBA(x, y, src.RGBAAt(x, sy))
		}
	}
	return out
}

// avgByte averages a byte sum with an explicit [0,255] bound: the uint8
// conversion can never overflow by construction.
func avgByte(sum, n int) uint8 {
	if n <= 0 || sum <= 0 {
		return 0
	}
	v := sum / n
	if v > 255 {
		v = 255
	}
	// #nosec G115 -- v is bounded to [0,255]: sum>0 and n>0 above, cap 255 here.
	return uint8(v)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// EncodePNG writes the image as PNG.
func EncodePNG(w io.Writer, img image.Image) error {
	enc := png.Encoder{CompressionLevel: png.DefaultCompression}
	if err := enc.Encode(w, img); err != nil {
		return fmt.Errorf("labelgen: encode png: %w", err)
	}
	return nil
}

// WriteImage saves img as dir/name. The name must be a flat file name: no
// separators and no ".." (output confinement).
func WriteImage(dir, name string, img image.Image) (string, error) {
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: %q", domain.ErrUnsafePath, name)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("labelgen: create dir: %w", err)
	}
	// os.OpenRoot confines every operation to dir at the OS level: neither a
	// malicious name nor a symlink can escape the destination directory.
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("labelgen: open root: %w", err)
	}
	defer root.Close()
	path := filepath.Join(dir, name)
	f, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("labelgen: create file: %w", err)
	}
	defer f.Close()
	if err := EncodePNG(f, img); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("labelgen: close file: %w", err)
	}
	return path, nil
}
