// Package icons draws the Cloudflare Tunnel app icons.
//
// Port of the old tools/make-icons.py, which used only zlib + struct so it
// could run on any fnOS box without Pillow or ImageMagick. The Go version uses
// only image/png from the standard library and keeps the same drawing: a
// rounded-square badge with a Cloudflare-orange vertical gradient, a white
// cloud, and a downward arrow (the "tunnel" motif), rendered with
// supersampling and un-premultiplied edges.
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

// Cloudflare orange gradient (top -> bottom), sRGB.
const (
	topR, topG, topB = 0xF9, 0xA1, 0x4A
	botR, botG, botB = 0xE0, 0x5A, 0x0B
	white            = 0xFF
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

					t := fy / big
					r := lerp(topR, botR, t)
					g := lerp(topG, botG, t)
					b := lerp(topB, botB, t)
					if cloudAndArrow(fx, fy, big) {
						r, g, b = white, white, white
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

// cloudAndArrow is the white foreground mask. Coordinates are normalised to
// 0..1 and then scaled by s.
func cloudAndArrow(x, y, s float64) bool {
	// --- cloud: three bumps over a flat base ---------------------------
	if inCircle(x, y, 0.325*s, 0.420*s, 0.110*s) {
		return true
	}
	if inCircle(x, y, 0.505*s, 0.360*s, 0.155*s) {
		return true
	}
	if inCircle(x, y, 0.670*s, 0.425*s, 0.110*s) {
		return true
	}
	if inRect(x, y, 0.215*s, 0.415*s, 0.785*s, 0.545*s) {
		return true
	}

	// --- downward arrow (the tunnel) -----------------------------------
	if inRect(x, y, 0.455*s, 0.525*s, 0.545*s, 0.670*s) {
		return true
	}
	return inTriangle(x, y, 0.365*s, 0.655*s, 0.635*s, 0.655*s, 0.500*s, 0.810*s)
}
