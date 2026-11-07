// Package icons draws the Cloudflare Tunnel app icons.
//
// Port of the old tools/make-icons.py, which used only zlib + struct so it
// could run on any fnOS box without Pillow or ImageMagick. The Go version uses
// only image/png from the standard library.
//
// The mark is a white cloud over a horizontal arrow, on a Cloudflare-orange
// gradient rounded square: the cloud sends traffic sideways into the tunnel.
//
// The arrow is horizontal on purpose. A vertical shaft hanging under a wide
// rounded canopy is the silhouette of a tree, and the earlier drafts of this
// icon read as foliage however the arrowhead was proportioned; the other
// attempts (chevrons, a separate arch, a tunnel mouth notched out of the cloud)
// all collapsed into two stacked blobs at 64px. Sideways keeps both the meaning
// and the silhouette.
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

// Cloudflare orange gradient (top -> bottom), sRGB, with a white glyph.
const (
	topR, topG, topB = 0xFA, 0xA5, 0x4F
	botR, botG, botB = 0xD5, 0x51, 0x0A
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

// foreground is the white mark: the cloud, plus the horizontal arrow that
// leaves it towards the tunnel.
func foreground(x, y, s float64) (float64, float64, float64, bool) {
	if inCloud(x, y, s) || inArrow(x, y, s) {
		return 255, 255, 255, true
	}
	return 0, 0, 0, false
}

// inCloud is the cloud silhouette: three overlapping domes over a rounded base.
// The base is rounded rather than square so the silhouette does not read as a
// slab with circles glued on top at small sizes.
func inCloud(x, y, s float64) bool {
	if inCircle(x, y, 0.330*s, 0.330*s, 0.115*s) {
		return true
	}
	if inCircle(x, y, 0.500*s, 0.262*s, 0.160*s) {
		return true
	}
	if inCircle(x, y, 0.670*s, 0.326*s, 0.108*s) {
		return true
	}
	return inRoundRectBox(x, y, 0.215*s, 0.312*s, 0.785*s, 0.448*s, 0.068*s)
}

// inArrow is a horizontal arrow under the cloud: a rounded bar with a solid
// head pointing right, i.e. traffic leaving the cloud for the tunnel.
//
// It is horizontal, not vertical, on purpose. A vertical shaft hanging under a
// wide rounded canopy is the silhouette of a tree, and the first drafts of this
// icon read as foliage whatever the arrowhead was proportioned like. Turning
// the arrow on its side keeps the meaning and kills the tree.
func inArrow(x, y, s float64) bool {
	if inRoundRectBox(x, y, 0.205*s, 0.545*s, 0.700*s, 0.655*s, 0.055*s) {
		return true
	}
	return inTriangle(x, y, 0.640*s, 0.500*s, 0.640*s, 0.700*s, 0.800*s, 0.600*s)
}
