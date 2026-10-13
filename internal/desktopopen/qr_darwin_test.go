//go:build darwin && cgo

package desktopopen

import (
	"bytes"
	"image/png"
	"testing"
)

func TestPairingQRProducesNativePNG(t *testing.T) {
	data := []byte(`{"version":1,"relay_url":"https://relay.test","grant":{"id":"pair_fixture","secret":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","modules":["module_a"],"expires_at":"2030-01-01T00:00:00Z"}}`)
	encoded, err := QRPNG(data)
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal("invalid PNG", err)
	}
	if image.Bounds().Dx() != image.Bounds().Dy() || image.Bounds().Dx() < 100 {
		t.Fatal("unusable QR size")
	}
	dark, light := 0, 0
	for y := image.Bounds().Min.Y; y < image.Bounds().Max.Y; y++ {
		for x := image.Bounds().Min.X; x < image.Bounds().Max.X; x++ {
			r, _, _, _ := image.At(x, y).RGBA()
			if r < 0x8000 {
				dark++
			} else {
				light++
			}
		}
	}
	if dark == 0 || light == 0 {
		t.Fatal("blank QR")
	}
	if _, err := QRPNG(make([]byte, 4097)); err == nil {
		t.Fatal("unbounded QR payload accepted")
	}
}
