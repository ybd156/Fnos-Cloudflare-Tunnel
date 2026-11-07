// Package icons draws the Cloudflare Tunnel app icons.
//
// Port of the old tools/make-icons.py, which used only zlib + struct so it
// could run on any fnOS box without Pillow or ImageMagick. The Go version uses
// only image/png from the standard library.
//
// The mark is a cloud with a tunnel mouth notched out of its base, drawn as a
// single flat silhouette in Cloudflare orange on a deep slate rounded square.
// Flat on purpose: no gradient, no bloom, no drop shadow.
//
// The shape went through several drafts. Anything that put a second glyph under
// the cloud -- an arrow, a chevron, a separate arch -- read as two stacked
// stickers at 64px, and the arrow variant read as a tree, because a shaft under
// a wide rounded canopy is the silhouette of a tree however the head is
// proportioned. Notching the tunnel mouth out of the cloud itself keeps one
// unmistakable silhouette at every size.
//
// Everything is drawn with supersampling and un-premultiplied edges, so the
// curves stay clean at 64px without any antialiasing library.
package icons

import (
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
)

// Outputs are the icon files the package needs, relative to cloudflared/.
// ICON.PNG and ICON_256.PNG are read by the app centre; the other four are the
// copies the UI serves.
var Outputs = []struct {
	Rel  string
	Size int
}{
	{"ICON.PNG", 64},
	{"ICON_256.PNG", 256},
	{"app/ui/images/icon_64.png", 64},
	{"app/ui/images/icon_256.png", 256},
	{"app/www/images/icon_64.png", 64},
	{"app/www/images/icon_256.png", 256},
}

// Flat two-colour palette: a deep slate badge with a single Cloudflare-orange
// glyph. No gradient, no bloom, no drop shadow -- the brief is a flat mark, and
// the orange-on-slate pairing is what keeps it recognisable as Cloudflare.
const (
	bgR, bgG, bgB = 0x12, 0x1C, 0x2E
	fgR, fgG, fgB = 0xF6, 0x82, 0x1F
)

// Generate writes every icon under root/cloudflared and reports one line each.
func Generate(root string, out io.Writer) error {
	project := filepath.Join(root, "cloudflared")
	for _, o := range Outputs {
		target := filepath.Join(project, filepath.FromSlash(o.Rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("cannot create %s: %w", filepath.Dir(o.Rel), err)
		}
		supersample := 4
		if o.Size <= 64 {
			// The small icon is displayed at 64px and has the fewest pixels to
			// hide stair-stepping, so it gets the denser sampling.
			supersample = 8
		}
		img := render(o.Size, supersample)
		n, err := writePNG(target, img)
		if err != nil {
			return fmt.Errorf("cannot write %s: %w", o.Rel, err)
		}
		fmt.Fprintf(out, "%-34s %dx%d  %7d bytes\n", o.Rel, o.Size, o.Size, n)
	}
	return nil
}

// render returns one icon, drawn at size*supersample and box-filtered down.
func render(size, supersample int) *image.RGBA {
	big := float64(size * supersample)
	radius := big * 0.225
	samples := float64(supersample * supersample)

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var rAcc, gAcc, bAcc, aAcc float64
			for sy := 0; sy < supersample; sy++ {
				for sx := 0; sx < supersample; sx++ {
					fx := float64(px*supersample+sx) + 0.5
					fy := float64(py*supersample+sy) + 0.5

					if !inRoundedRect(fx, fy, big, radius) {
						continue
					}

					r, g, b := float64(bgR), float64(bgG), float64(bgB)
					if fr, fg, fb, ok := foreground(fx, fy, big); ok {
						r, g, b = fr, fg, fb
					}
					rAcc += r
					gAcc += g
					bAcc += b
					aAcc += 255
				}
			}

			alpha := aAcc / samples
			off := py*img.Stride + px*4
			if alpha <= 0 {
				img.Pix[off], img.Pix[off+1], img.Pix[off+2], img.Pix[off+3] = 0, 0, 0, 0
				continue
			}
			// Un-premultiply so partially covered edge pixels keep a clean
			// colour instead of fading towards black.
			scale := aAcc / 255
			img.Pix[off] = clamp255(rAcc / scale)
			img.Pix[off+1] = clamp255(gAcc / scale)
			img.Pix[off+2] = clamp255(bAcc / scale)
			img.Pix[off+3] = uint8(math.Round(alpha))
		}
	}
	return img
}

func writePNG(path string, img *image.RGBA) (int, error) {
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(f, img); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	_ = os.Chmod(path, 0o644)
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return int(st.Size()), nil
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func clamp255(v float64) uint8 {
	if v > 255 {
		v = 255
	}
	if v < 0 {
		v = 0
	}
	return uint8(math.Round(v))
}

func inRoundedRect(x, y, size, radius float64) bool {
	half := size / 2
	dx := math.Abs(x-half) - (half - radius)
	dy := math.Abs(y-half) - (half - radius)
	dx = math.Max(dx, 0)
	dy = math.Max(dy, 0)
	return dx*dx+dy*dy <= radius*radius
}

func inCircle(x, y, cx, cy, r float64) bool {
	dx := x - cx
	dy := y - cy
	return dx*dx+dy*dy <= r*r
}

func inRect(x, y, x0, y0, x1, y1 float64) bool {
	return x >= x0 && x <= x1 && y >= y0 && y <= y1
}

func inTriangle(x, y, p0x, p0y, p1x, p1y, p2x, p2y float64) bool {
	cross := func(ax, ay, bx, by, px, py float64) float64 {
		return (bx-ax)*(py-ay) - (by-ay)*(px-ax)
	}
	d1 := cross(p0x, p0y, p1x, p1y, x, y)
	d2 := cross(p1x, p1y, p2x, p2y, x, y)
	d3 := cross(p2x, p2y, p0x, p0y, x, y)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

// inRoundRectBox is a rounded rectangle given by two corners plus a corner
// radius. inRoundedRect only knows squares centred on the canvas, and the
// cloud's base is neither square nor centred.
func inRoundRectBox(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Min(math.Max(x, x0+r), x1-r)
	cy := math.Min(math.Max(y, y0+r), y1-r)
	dx := x - cx
	dy := y - cy
	return dx*dx+dy*dy <= r*r
}

// foreground is the mark, painted in a single flat colour.
func foreground(x, y, s float64) (float64, float64, float64, bool) {
	if inCloud(x, y, s) {
		return float64(fgR), float64(fgG), float64(fgB), true
	}
	return 0, 0, 0, false
}

// inCloud is the whole mark: three overlapping domes over a rounded base, in
// one flat colour.
//
// Earlier drafts tried to spell out "tunnel" inside the glyph -- an arrow under
// the cloud (which read as a tree: a shaft under a wide rounded canopy is the
// silhouette of a tree however the head is proportioned), a chevron, a separate
// arch, and finally a tunnel mouth notched out of the cloud's base. Every one
// of them either became two stacked stickers at 64px or turned the cloud into a
// blob with a bite taken out of it. The badge is dark and the glyph is one
// colour; the app is called Cloudflare Tunnel, and the cloud carries the brand
// far better than any second symbol did.
func inCloud(x, y, s float64) bool {
	// Map the canvas onto the mark's own 0..1 box, zoomed in and nudged down:
	// an app icon reads better with the glyph filling most of the badge, and
	// optical centring sits it slightly below the geometric middle.
	const zoom = 1.16
	const shiftY = 0.045
	nx := (x/s-0.5)/zoom + 0.5
	ny := (y/s-(0.5+shiftY))/zoom + 0.5

	return inCircle(nx*s, ny*s, 0.295*s, 0.445*s, 0.140*s) ||
		inCircle(nx*s, ny*s, 0.500*s, 0.362*s, 0.192*s) ||
		inCircle(nx*s, ny*s, 0.705*s, 0.440*s, 0.133*s) ||
		inRoundRectBox(nx*s, ny*s, 0.160*s, 0.418*s, 0.840*s, 0.615*s, 0.090*s)
}
