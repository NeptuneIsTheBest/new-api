package requestplugin

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/image/draw"
)

// Each iteration processes the same images. EndToEnd runs an outbound plugin
// and measures the actual JSON sent upstream, including unchanged media.
func BenchmarkWebPMedia(b *testing.B) {
	read := func(path string) []byte {
		data, err := os.ReadFile(path)
		require.NoError(b, err)
		return data
	}
	dashboard, err := jpeg.Decode(bytes.NewReader(read("../../assets/screenshots/dashboard.en.jpg")))
	require.NoError(b, err)
	screen := image.NewRGBA(image.Rect(0, 0, 960, 540))
	draw.ApproxBiLinear.Scale(screen, screen.Bounds(), dashboard, dashboard.Bounds(), draw.Src, nil)
	var screenshot bytes.Buffer
	require.NoError(b, png.Encode(&screenshot, screen))

	// Use the independent VP8, VP8+ALPH and VP8L fixtures shipped with our
	// pinned x/image dependency, so neither codec generates its own input.
	moduleDir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "golang.org/x/image").Output()
	require.NoError(b, err)
	fixtures := filepath.Join(strings.TrimSpace(string(moduleDir)), "testdata")
	files := [][]byte{
		screenshot.Bytes(),
		read("../../web/public/logo.png"),
		read(filepath.Join(runtime.GOROOT(), "src/image/testdata/video-001.gif")),
		read(filepath.Join(fixtures, "blue-purple-pink-large.normal-filter.lossy.webp")),
		read(filepath.Join(fixtures, "yellow_rose.lossy-with-alpha.webp")),
		read(filepath.Join(fixtures, "blue-purple-pink-large.lossless.webp")),
	}

	var sources []image.Image
	type webpInput struct {
		data   []byte
		config image.Config
	}
	var webps []webpInput
	var parts, effects []string
	for i, file := range files {
		config, format, err := image.DecodeConfig(bytes.NewReader(file))
		require.NoError(b, err)
		target := `"format":"webp"`
		if format == "webp" {
			webps = append(webps, webpInput{file, config})
			target = `"format":"auto","maxSide":256`
		} else {
			img, _, err := image.Decode(bytes.NewReader(file))
			require.NoError(b, err)
			sources = append(sources, img)
		}
		parts = append(parts, `{"type":"image_url","image_url":{"url":"`+dataURI("image/"+format, file)+`"}}`)
		effects = append(effects, fmt.Sprintf(`{"type":"media.reencode","media":%d,%s}`, i, target))
	}

	b.Run("Encode", func(b *testing.B) {
		for b.Loop() {
			for _, img := range sources {
				var encoded bytes.Buffer
				if err := encodeLosslessWebP(b.Context(), &encoded, img); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("Gray16", func(b *testing.B) {
		for _, side := range []int{64, 256} {
			bounds := image.Rect(0, 0, side, side)
			gray := image.NewGray16(bounds)
			for y := range side {
				for x := range side {
					gray.SetGray16(x, y, color.Gray16{Y: uint16(x)<<8 | uint16(y)})
				}
			}
			nrgba := image.NewNRGBA(bounds)
			draw.Draw(nrgba, bounds, gray, bounds.Min, draw.Src)
			for _, input := range []struct {
				name string
				img  image.Image
			}{{"Gray16", gray}, {"NRGBA", nrgba}} {
				b.Run(fmt.Sprintf("%dx%d/%s", side, side, input.name), func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if err := encodeLosslessWebP(b.Context(), io.Discard, input.img); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	})
	b.Run("Decode", func(b *testing.B) {
		for b.Loop() {
			for _, input := range webps {
				if _, err := decodeWebP(b.Context(), input.data, input.config); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("EndToEnd", func(b *testing.B) {
		setupPlugins(b, `[{"id":"bench","name":"Bench","plugin":"rp-stages","enabled":true,"global":true,"config":{"effects":{"outbound":[`+strings.Join(effects, ",")+`]}}}]`)
		call := Call{Stage: StageOutbound, Body: []byte(chatBody(parts...))}
		require.NotNil(b, NewPlan())
		var size int
		for b.Loop() {
			out := NewPlan().Run(b.Context(), call)
			if out.Failed != nil {
				b.Fatal(out.Failed.Detail)
			}
			size = len(out.Body)
		}
		b.ReportMetric(float64(size), "body-B/op")
	})
}
