package requestplugin

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"strings"

	"github.com/HugoSmits86/nativewebp"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/requestpatch"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/kolesa-team/go-webp/v2/decoder"
	"github.com/kolesa-team/go-webp/v2/encoder"
	webputils "github.com/kolesa-team/go-webp/v2/utils"
	"github.com/tidwall/gjson"
	"golang.org/x/image/draw"
	"golang.org/x/image/riff"
	_ "golang.org/x/image/webp"
)

// Limits of media.reencode.
const (
	maxReencodeBytes  = 20 << 20
	maxReencodePixels = 40_000_000
)

// Level 0 gave the fastest outbound request within the previous encoder's
// body size on the BenchmarkWebPMedia corpus.
const webpLosslessLevel = 0

// reencodeSlots bounds the re-encodes running at once in the process.
var reencodeSlots = make(chan struct{}, 4)

// inlineMedia is one inline media string of a JSON body: a base64 data URL,
// or the data of a Claude base64 source, a Gemini inline data part or a chat
// input_audio part, whose mime type or audio format is a sibling member.
type inlineMedia struct {
	// start and end bound the quoted string in the original JSON.
	start, end int
	// path is the JSON Pointer of the media string.
	path string
	// mimePath is the JSON Pointer of the sibling mime type member; empty
	// for a data URL and for input_audio.
	mimePath string
	mime     string
	// data is the base64 payload.
	data string
	// filename is the sibling filename member of a chat or Responses file.
	filename string
	// audio marks input_audio data, whose format names its mime type.
	audio bool
}

// size is the decoded byte length of the payload.
func (m inlineMedia) size() int {
	return len(m.data)*3/4 - strings.Count(m.data[max(0, len(m.data)-2):], "=")
}

// dimensions reads the pixel size of an image from its header; zero when the
// media is not an image the host decodes.
func (m inlineMedia) dimensions() (int, int) {
	if !strings.HasPrefix(m.mime, "image/") {
		return 0, 0
	}
	config, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(m.data)))
	if err != nil {
		return 0, 0
	}
	return config.Width, config.Height
}

// scanMedia finds the inline media of a JSON body and returns the body with
// each media string replaced by "[media:N]", the view a stage hook reads.
func scanMedia(body []byte) ([]byte, []inlineMedia) {
	if !bytes.Contains(body, []byte("base64")) && !bytes.Contains(body, []byte("nlineData")) && !bytes.Contains(body, []byte("nline_data")) && !bytes.Contains(body, []byte("input_audio")) {
		return body, nil
	}
	source := string(body)
	var media []inlineMedia
	compactSize := walkMedia(gjson.Parse(source), "", "", &media)
	if len(media) == 0 {
		return body, nil
	}

	viewSize := compactSize
	for index, item := range media {
		viewSize += len(mediaRef(index)) + 2 - (item.end - item.start)
	}
	view := make([]byte, 0, viewSize)
	compact := compactSize == len(source)
	offset := 0
	for index, item := range media {
		view = appendMediaViewSpan(view, source[offset:item.start], compact)
		view = append(view, '"')
		view = append(view, mediaRef(index)...)
		view = append(view, '"')
		offset = item.end
	}
	return appendMediaViewSpan(view, source[offset:], compact), media
}

// appendMediaViewSpan copies a range between media strings into the compact view.
func appendMediaViewSpan(view []byte, span string, compact bool) []byte {
	if compact {
		return append(view, span...)
	}
	offset := 0
	forEachJSONString(span, func(start, end int, _ bool) {
		view = appendJSONSyntax(view, span[offset:start])
		view = append(view, span[start:end]...)
		offset = end
	})
	return appendJSONSyntax(view, span[offset:])
}

// appendJSONSyntax copies the non-string tokens between quoted JSON strings.
func appendJSONSyntax(dst []byte, syntax string) []byte {
	for i := range len(syntax) {
		if syntax[i] > ' ' {
			dst = append(dst, syntax[i])
		}
	}
	return dst
}

// walkMedia records inline media in source order and returns the compact byte
// length of the original value, including its quoted media strings.
func walkMedia(value gjson.Result, pointer, key string, media *[]inlineMedia) int {
	switch {
	case value.IsObject():
		var mimeMember, mime string
		var checkedMIME bool
		size := 2 // braces
		first := true
		own := len(*media)
		filename := ""
		value.ForEach(func(name, member gjson.Result) bool {
			if !first {
				size++ // comma
			}
			first = false
			size += len(name.Raw) + 1 // key and colon
			memberPointer := pointer + "/" + escapePointer(name.Str)
			isData := name.Str == "data" && member.Type == gjson.String
			if isData && !checkedMIME {
				mimeMember, mime = inlineMediaMIME(value, key)
				checkedMIME = true
			}
			if isData && mimeMember != "" {
				item := inlineMedia{path: memberPointer, mimePath: pointer + "/" + mimeMember, mime: mime, data: member.Str}
				if mimeMember == "format" {
					item.mimePath, item.mime, item.audio = "", "audio/"+strings.ToLower(item.mime), true
				}
				item.start, item.end = member.Index, member.Index+len(member.Raw)
				*media = append(*media, item)
				size += len(member.Raw)
				return true
			}
			if name.Str == "filename" && member.Type == gjson.String {
				filename = member.Str
			}
			size += walkMedia(member, memberPointer, name.Str, media)
			return true
		})
		// A file's name is a sibling of its data URL.
		for index := own; filename != "" && index < len(*media); index++ {
			if item := &(*media)[index]; strings.LastIndexByte(item.path, '/') == len(pointer) && strings.HasPrefix(item.path, pointer) {
				item.filename = filename
			}
		}
		return size
	case value.IsArray():
		size := 2 // brackets
		index := 0
		value.ForEach(func(_, item gjson.Result) bool {
			if index > 0 {
				size++ // comma
			}
			size += walkMedia(item, fmt.Sprintf("%s/%d", pointer, index), "", media)
			index++
			return true
		})
		return size
	case value.Type == gjson.String:
		if header, data, found := strings.Cut(value.Str, ","); found && strings.HasPrefix(header, "data:") && strings.HasSuffix(header, ";base64") {
			mime, _, _ := strings.Cut(strings.TrimPrefix(header, "data:"), ";")
			*media = append(*media, inlineMedia{
				path: pointer, mime: mime, data: data,
				start: value.Index, end: value.Index + len(value.Raw),
			})
		}
	}
	return len(value.Raw)
}

// inlineMediaMIME returns the name and value of the MIME or audio format
// field describing a sibling data string.
func inlineMediaMIME(object gjson.Result, key string) (string, string) {
	if object.Get("type").Str == "base64" {
		if mime := object.Get("media_type"); mime.Type == gjson.String {
			return "media_type", mime.Str
		}
	}
	switch key {
	case "inlineData", "inline_data":
		for _, field := range []string{"mime_type", "mimeType"} {
			if mime := object.Get(field); mime.Type == gjson.String {
				return field, mime.Str
			}
		}
	case "input_audio":
		if format := object.Get("format"); format.Type == gjson.String {
			return "format", format.Str
		}
	}
	return "", ""
}

func escapePointer(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

// reencodeMedia decodes an inline image and encodes it again as a
// media.reencode effect asks. It returns the json.patch operations that
// replace the media string and its mime type, and the change for the public
// record; none when the new bytes are not smaller or the image is an
// animated GIF or over the limits.
func reencodeMedia(ctx context.Context, item inlineMedia, effect requestpatch.Effect) ([]requestpatch.Operation, *relaycommon.RequestPluginMediaChange, error) {
	if item.size() > maxReencodeBytes {
		return nil, nil, nil
	}
	select {
	case reencodeSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-reencodeSlots }()

	raw, err := base64.StdEncoding.DecodeString(item.data)
	if err != nil {
		return nil, nil, fmt.Errorf("media %s is not valid base64", item.path)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, fmt.Errorf("media %s is not a jpeg, png, gif or webp image", item.path)
	}
	if int64(config.Width)*int64(config.Height) > maxReencodePixels || format == "gif" && gifAnimated(raw) {
		return nil, nil, nil
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, err
	}
	var img image.Image
	if format == "webp" {
		img, err = decodeWebP(ctx, raw, config)
	} else {
		img, _, err = image.Decode(bytes.NewReader(raw))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, ctxErr
	}
	if err != nil {
		return nil, nil, fmt.Errorf("media %s cannot be decoded", item.path)
	}
	bounds := img.Bounds()
	if longest := max(bounds.Dx(), bounds.Dy()); effect.MaxSide > 0 && longest > effect.MaxSide {
		width := max(1, bounds.Dx()*effect.MaxSide/longest)
		height := max(1, bounds.Dy()*effect.MaxSide/longest)
		scaledBounds := image.Rect(0, 0, width, height)
		var scaled draw.Image
		if format == "jpeg" && (effect.Format == "jpeg" || effect.Format == "auto") {
			// JPEG input is opaque, and jpeg.Encode reads RGBA pixels directly.
			scaled = image.NewRGBA(scaledBounds)
		} else {
			scaled = image.NewNRGBA(scaledBounds)
		}
		draw.CatmullRom.Scale(scaled, scaledBounds, img, bounds, draw.Src, nil)
		img = scaled
	}
	target := effect.Format
	opaque, known := img.(interface{ Opaque() bool })
	transparent := (target == "auto" || target == "jpeg") && (!known || !opaque.Opaque())
	if target == "auto" {
		target = "jpeg"
		if transparent {
			target = "png"
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, err
	}
	var encoded bytes.Buffer
	switch target {
	case "jpeg":
		if transparent {
			canvas := image.NewRGBA(img.Bounds())
			draw.Draw(canvas, canvas.Bounds(), image.White, image.Point{}, draw.Src)
			draw.Draw(canvas, canvas.Bounds(), img, img.Bounds().Min, draw.Over)
			img = canvas
		}
		quality := effect.Quality
		if quality == 0 {
			quality = jpeg.DefaultQuality
		}
		err = jpeg.Encode(&encoded, img, &jpeg.Options{Quality: quality})
	case "png":
		err = png.Encode(&encoded, img)
	case "webp":
		err = encodeLosslessWebP(ctx, &encoded, img)
	default:
		return nil, nil, fmt.Errorf("format %q is not supported", effect.Format)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, ctxErr
	}
	if err != nil {
		return nil, nil, fmt.Errorf("media %s cannot be encoded as %s", item.path, target)
	}
	if encoded.Len() >= len(raw) {
		return nil, nil, nil
	}

	mime := "image/" + target
	payload := base64.StdEncoding.EncodeToString(encoded.Bytes())
	if item.mimePath == "" {
		payload = "data:" + mime + ";base64," + payload
	}
	value, err := common.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	ops := []requestpatch.Operation{{Op: "replace", Path: item.path, Value: value}}
	if item.mimePath != "" {
		mimeValue, err := common.Marshal(mime)
		if err != nil {
			return nil, nil, err
		}
		ops = append(ops, requestpatch.Operation{Op: "replace", Path: item.mimePath, Value: mimeValue})
	}
	return ops, &relaycommon.RequestPluginMediaChange{
		From:     relaycommon.RequestPluginMediaForm{Mime: item.mime, Bytes: len(raw)},
		To:       relaycommon.RequestPluginMediaForm{Mime: mime, Bytes: encoded.Len()},
		Lossless: target != "jpeg",
	}, nil
}

func decodeWebP(ctx context.Context, data []byte, config image.Config) (image.Image, error) {
	dec, err := decoder.NewDecoder(webpWithVP8XFirst(data), nil)
	if err != nil {
		return nil, err
	}
	// The decoder validates the VP8/VP8L frame against its VP8X canvas.
	// Check that these are the dimensions admitted by the host's pixel limit.
	features := dec.GetFeatures()
	if features.Width != config.Width || features.Height != config.Height {
		return nil, errors.New("WebP frame dimensions differ from the image header")
	}
	if features.Format == webputils.FormatLossy {
		if features.HasAlpha {
			return dec.DecodeNYCbCrA()
		}
		return dec.DecodeYCbCr()
	}
	return dec.DecodeContext(ctx)
}

// webpWithVP8XFirst presents VP8X before leading XMP for go-webp.
// RFC 9649 §2.7 allows metadata and unknown chunks to precede VP8X.
func webpWithVP8XFirst(data []byte) io.Reader {
	const (
		riffHeaderSize  = 12
		chunkHeaderSize = 8
		vp8xPayloadSize = 10
	)
	original := bytes.NewReader(data)
	if len(data) < riffHeaderSize+4 {
		return original
	}
	switch string(data[riffHeaderSize : riffHeaderSize+4]) {
	case "VP8X", "VP8 ", "VP8L":
		return original
	}

	cursor := bytes.NewReader(data)
	form, chunks, err := riff.NewReader(cursor)
	if err != nil || string(form[:]) != "WEBP" {
		return original
	}
	var leadingXMP bool
	for {
		id, size, _, err := chunks.Next()
		if err != nil {
			return original
		}
		switch string(id[:]) {
		case "XMP ":
			leadingXMP = true
		case "VP8X":
			if !leadingXMP || size != vp8xPayloadSize || cursor.Len() < vp8xPayloadSize {
				return original
			}
			// Next leaves the cursor at the start of the VP8X payload.
			payloadStart := len(data) - cursor.Len()
			start, end := payloadStart-chunkHeaderSize, payloadStart+vp8xPayloadSize
			return io.MultiReader(
				bytes.NewReader(data[:riffHeaderSize]),
				bytes.NewReader(data[start:end]),
				bytes.NewReader(data[riffHeaderSize:start]),
				bytes.NewReader(data[end:]),
			)
		case "ICCP", "ANIM", "ANMF", "ALPH", "VP8 ", "VP8L":
			// Color and image reconstruction chunks have a required order.
			return original
		}
	}
}

func encodeLosslessWebP(ctx context.Context, w io.Writer, img image.Image) error {
	const maxVP8LSide = 1 << 14
	size := img.Bounds().Size()
	longest := max(size.X, size.Y)
	// VP8L can represent 16384 pixels per side. go-webp encodes up to 16383.
	if longest == maxVP8LSide {
		return nativewebp.Encode(w, img, nil)
	}
	if gray, ok := img.(*image.Gray16); ok && min(size.X, size.Y) > 0 && longest < maxVP8LSide {
		// Draw uses RGBA64At and SetRGBA64, avoiding go-webp's per-pixel color allocations.
		nrgba := image.NewNRGBA(image.Rectangle{Max: size})
		draw.Draw(nrgba, nrgba.Bounds(), gray, gray.Bounds().Min, draw.Src)
		img = nrgba
	}
	options, err := encoder.NewLosslessEncoderOptions(encoder.PresetDefault, webpLosslessLevel)
	if err != nil {
		return err
	}
	options.Exact = 1 // Preserve RGB under fully transparent pixels.
	enc, err := encoder.NewEncoder(img, options)
	if err != nil {
		return err
	}
	defer enc.Release()
	return enc.EncodeContext(ctx, w)
}

// gifAnimated reports a GIF with more than one image. It walks the block
// structure without decoding any pixels.
func gifAnimated(data []byte) bool {
	if len(data) < 13 {
		return false
	}
	position := 13
	if data[10]&0x80 != 0 {
		position += 3 << (data[10]&7 + 1)
	}
	frames := 0
	for position < len(data) {
		switch data[position] {
		case 0x21: // extension: introducer and label, then data sub-blocks
			position += 2
		case 0x2C: // image descriptor
			if frames++; frames > 1 {
				return true
			}
			if position+10 > len(data) {
				return false
			}
			flags := data[position+9]
			position += 10
			if flags&0x80 != 0 {
				position += 3 << (flags&7 + 1)
			}
			position++ // LZW minimum code size
		default: // trailer
			return false
		}
		for position < len(data) && data[position] != 0 {
			position += int(data[position]) + 1
		}
		position++
	}
	return false
}
