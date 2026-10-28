package icons

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateWritesSixValidPNGs(t *testing.T) {
	root := t.TempDir()
	var out bytes.Buffer
	if err := Generate(root, &out); err != nil {
		t.Fatalf("generate: %v", err)
	}

	for _, o := range Outputs {
		path := filepath.Join(root, "cloudflared", filepath.FromSlash(o.Rel))
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("%s was not written: %v", o.Rel, err)
		}
		cfg, err := png.DecodeConfig(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s is not a PNG: %v", o.Rel, err)
		}
		if cfg.Width != o.Size || cfg.Height != o.Size {
			t.Fatalf("%s is %dx%d, want %dx%d", o.Rel, cfg.Width, cfg.Height, o.Size, o.Size)
		}
		if cfg.ColorModel != color.RGBAModel && cfg.ColorModel != color.NRGBAModel {
			// The icons need an alpha channel: anything else means the encoder
			// dropped transparency and the badge would render on a black box.
			t.Fatalf("%s is not an alpha-capable image: %T", o.Rel, cfg.ColorModel)
		}

		decoded, err := decode(path)
		if err != nil {
			t.Fatal(err)
		}
		// A rounded square leaves the corners fully transparent.
		if _, _, _, a := decoded.At(0, 0).RGBA(); a != 0 {
			t.Fatalf("%s: the corner is not transparent (alpha=%d)", o.Rel, a)
		}
		// The centre is inside the orange badge and must be opaque.
		if _, _, _, a := decoded.At(o.Size/2, o.Size/2).RGBA(); a != 0xffff {
			t.Fatalf("%s: the centre is not opaque (alpha=%d)", o.Rel, a)
		}
	}
}

func TestGenerateIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := Generate(root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(root, "cloudflared", "ICON.PNG"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(root, "cloudflared", "ICON.PNG"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("two runs produced different bytes for ICON.PNG")
	}
}

func decode(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}
