package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // register decoder
	"image/jpeg"
	_ "image/png" // register decoder
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register decoder

	"github.com/atb00ker/CreditAPI/src/internal/claude"
)

const (
	maxImageDownload = 20 << 20
	// The Messages API rejects images over 5 MB once base64 encoded.
	maxImageBytes = 3_700_000
)

// ImageProcessor turns OpenAI image_url values into Claude image blocks.
type ImageProcessor struct {
	MaxDim       int
	AllowURLs    bool
	AllowPrivate bool
	client       *http.Client
}

// NewImageProcessor builds a processor with an SSRF-guarded HTTP client.
func NewImageProcessor(maxDim int, allowURLs, allowPrivate bool) *ImageProcessor {
	p := &ImageProcessor{MaxDim: maxDim, AllowURLs: allowURLs, AllowPrivate: allowPrivate}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		dialer.Control = func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || !isPublicIP(ip) {
				return fmt.Errorf("refusing to fetch image from non-public address %s", host)
			}
			return nil
		}
	}
	p.client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:                 nil, // direct, so the address check above applies
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			MaxIdleConns:          4,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
	return p
}

func isPublicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() ||
		// carrier-grade NAT 100.64.0.0/10
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64))
}

// Load converts one image reference into a Claude image block.
func (p *ImageProcessor) Load(ctx context.Context, ref string) (claude.ContentBlock, error) {
	data, err := p.fetch(ctx, strings.TrimSpace(ref))
	if err != nil {
		return claude.ContentBlock{}, err
	}
	data, mediaType, err := p.normalize(data)
	if err != nil {
		return claude.ContentBlock{}, err
	}
	return claude.ContentBlock{
		Type:   "image",
		Source: &claude.ImageSource{Type: "base64", MediaType: mediaType, Data: base64.StdEncoding.EncodeToString(data)},
	}, nil
}

func (p *ImageProcessor) fetch(ctx context.Context, ref string) ([]byte, error) {
	switch {
	case strings.HasPrefix(ref, "data:"):
		meta, payload, ok := strings.Cut(ref[len("data:"):], ",")
		if !ok {
			return nil, errors.New("malformed data URL")
		}
		if strings.HasSuffix(strings.ToLower(meta), ";base64") {
			return decodeBase64(payload)
		}
		s, err := url.PathUnescape(payload)
		return []byte(s), err
	case strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://"):
		if !p.AllowURLs {
			return nil, errors.New("image URLs are disabled on this server (ALLOW_IMAGE_URLS=false); send the image as a base64 data URL")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "creditapi")
		resp, err := p.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download image: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download image: HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxImageDownload+1))
		if err != nil {
			return nil, fmt.Errorf("download image: %w", err)
		}
		if len(b) > maxImageDownload {
			return nil, errors.New("image is larger than 20 MB")
		}
		return b, nil
	default:
		// Some clients send bare base64 without the data: prefix.
		if len(ref) > 64 {
			if b, err := decodeBase64(ref); err == nil {
				return b, nil
			}
		}
		return nil, errors.New("image_url must be a data URL (data:image/jpeg;base64,...), base64 data, or an http(s) URL")
	}
}

func decodeBase64(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64 image data")
}

func sniff(data []byte) string {
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		brand := string(data[8:12])
		switch brand {
		case "heic", "heix", "hevc", "heim", "heis", "mif1", "msf1":
			return "image/heic"
		case "avif", "avis":
			return "image/avif"
		}
	}
	return http.DetectContentType(data)
}

// normalize validates the image and re-encodes it when it is too large,
// rotated via EXIF, or larger than MaxDim.
func (p *ImageProcessor) normalize(data []byte) ([]byte, string, error) {
	mediaType := sniff(data)
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	case "image/heic", "image/avif":
		return nil, "", fmt.Errorf("%s images are not supported; please send JPEG, PNG, GIF or WebP (most phones can share photos as JPEG)", strings.TrimPrefix(mediaType, "image/"))
	default:
		return nil, "", fmt.Errorf("unsupported image format %q; send JPEG, PNG, GIF or WebP", mediaType)
	}

	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("cannot read image: %w", err)
	}
	orientation := 1
	if mediaType == "image/jpeg" {
		orientation = jpegOrientation(data)
	}
	longest := max(cfg.Width, cfg.Height)
	tooLargeDims := p.MaxDim > 0 && longest > p.MaxDim
	if !tooLargeDims && orientation <= 1 && len(data) <= maxImageBytes {
		return data, mediaType, nil
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("cannot decode image: %w", err)
	}
	target := longest
	if tooLargeDims {
		target = p.MaxDim
	}
	for attempt := 0; attempt < 4; attempt++ {
		img := scaleToFit(src, target)
		img = applyOrientation(img, orientation)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
			return nil, "", err
		}
		if buf.Len() <= maxImageBytes {
			return buf.Bytes(), "image/jpeg", nil
		}
		target = target * 3 / 4
	}
	return nil, "", errors.New("image is too large even after resizing")
}

// scaleToFit returns an opaque RGBA copy of src whose longest side is at most
// maxSide, composited over white so transparent areas do not turn black.
func scaleToFit(src image.Image, maxSide int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if maxSide > 0 && max(w, h) > maxSide {
		if w >= h {
			h = max(1, h*maxSide/w)
			w = maxSide
		} else {
			w = max(1, w*maxSide/h)
			h = maxSide
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	if w == b.Dx() && h == b.Dy() {
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	} else {
		draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	}
	return dst
}

// applyOrientation rotates/flips img according to an EXIF orientation value.
func applyOrientation(img *image.RGBA, o int) *image.RGBA {
	if o < 2 || o > 8 {
		return img
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		for dx := 0; dx < dw; dx++ {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-dx, dy
			case 3:
				sx, sy = w-1-dx, h-1-dy
			case 4:
				sx, sy = dx, h-1-dy
			case 5:
				sx, sy = dy, dx
			case 6:
				sx, sy = dy, h-1-dx
			case 7:
				sx, sy = w-1-dy, h-1-dx
			case 8:
				sx, sy = w-1-dy, dx
			}
			si := img.PixOffset(sx, sy)
			di := dst.PixOffset(dx, dy)
			copy(dst.Pix[di:di+4], img.Pix[si:si+4])
		}
	}
	return dst
}

// jpegOrientation returns the EXIF orientation (1-8) of a JPEG, or 0.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 0
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 0
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF:
			i++
			continue
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8):
			i += 2
			continue
		case marker == 0xDA || marker == 0xD9:
			return 0
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		if segLen < 2 || i+2+segLen > len(b) {
			return 0
		}
		if marker == 0xE1 {
			seg := b[i+4 : i+2+segLen]
			if len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
				return tiffOrientation(seg[6:])
			}
		}
		i += 2 + segLen
	}
	return 0
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(t[2:4]) != 42 {
		return 0
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 0
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 0
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
	}
	return 0
}
