package web

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"sync"
)

// Tiny generated assets, so the image needs no files: Twitter's default
// profile background colour and a simple default avatar at each size.

var (
	assetsOnce sync.Once
	assets     map[string][]byte
)

func loadAssets() map[string][]byte {
	assetsOnce.Do(func() {
		assets = map[string][]byte{}
		// 1x1 background tile in Twitter's default 9ae4e8.
		pal := color.Palette{color.RGBA{0x9a, 0xe4, 0xe8, 0xff}}
		var g bytes.Buffer
		gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 1, 1), pal), nil)
		assets["bg.gif"] = g.Bytes()
		for name, size := range map[string]int{
			"default_profile.png": 128, "default_profile_normal.png": 48,
			"default_profile_bigger.png": 73, "default_profile_mini.png": 24,
		} {
			assets[name] = avatar(size)
		}
		assets["favicon.ico"] = avatar(16)
	})
	return assets
}

// avatar draws a sky-blue square with a white circle "head and shoulders".
func avatar(n int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	bg := color.RGBA{0x33, 0x99, 0xcc, 0xff}
	fg := color.RGBA{0xff, 0xff, 0xff, 0xff}
	cx, cy := float64(n)/2, float64(n)*0.4
	r := float64(n) * 0.2
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			c := bg
			if (fx-cx)*(fx-cx)+(fy-cy)*(fy-cy) <= r*r {
				c = fg
			}
			// shoulders: lower ellipse
			sy := float64(n) * 1.05
			if (fx-cx)*(fx-cx)/(float64(n)*0.38*float64(n)*0.38)+(fy-sy)*(fy-sy)/(float64(n)*0.42*float64(n)*0.42) <= 1 {
				c = fg
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}
