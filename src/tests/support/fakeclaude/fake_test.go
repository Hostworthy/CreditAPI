package fakeclaude

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"testing"
)

func pngBase64(t *testing.T, w, h int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestDescribeImages(t *testing.T) {
	blocks := []block{
		{Type: "text", Text: "hi"},
		{Type: "image", Source: &imageSource{MediaType: "image/png", Data: pngBase64(t, 2, 3)}},
		{Type: "image", Source: &imageSource{MediaType: "image/jpeg", Data: "!!!"}},
	}
	if got := describeImages(blocks); got != "I see 2 image(s): image/png 2x3, image/jpeg unreadable." {
		t.Fatalf("%q", got)
	}
	if describeImages(blocks[:1]) != "" || countImages(blocks) != 2 {
		t.Fatal("image counting")
	}
}
