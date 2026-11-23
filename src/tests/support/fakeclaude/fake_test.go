package fakeclaude

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
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

func TestSampleFromSchema(t *testing.T) {
	var schema map[string]any
	_ = json.Unmarshal([]byte(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"meal_type": {"type": "string", "enum": ["lunch", "dinner"]},
			"kind": {"const": "food"},
			"date": {"type": "string", "format": "date"},
			"at": {"type": "string", "format": "date-time"},
			"email": {"type": "string", "format": "email"},
			"grams": {"type": "number"},
			"count": {"type": "integer"},
			"ok": {"type": "boolean"},
			"note": {"type": ["null", "string"]},
			"tags": {"type": "array", "items": {"type": "string"}},
			"macro": {"properties": {"protein": {"type": "number"}}},
			"either": {"anyOf": [{"type": "integer"}, {"type": "string"}]},
			"nothing": {"type": "null"}
		}
	}`), &schema)
	got, _ := json.Marshal(SampleFromSchema(schema, "root"))
	want := `{"at":"2026-01-01T12:00:00Z","count":42,"date":"2026-01-01","either":42,"email":"fake@example.com","grams":42,"kind":"food","macro":{"protein":42},"meal_type":"lunch","name":"fake name","note":"fake note","nothing":null,"ok":true,"tags":["fake tags"]}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if v, _ := json.Marshal(SampleFromSchema(nil, "x")); string(v) != "{}" {
		t.Fatalf("nil schema: %s", v)
	}
}

func TestDirectives(t *testing.T) {
	d := directives("please [fake:slow] and [fake:echo-input] [fake:Bad]")
	if !d["slow"] || !d["echo-input"] || d["bad"] || len(d) != 2 {
		t.Fatalf("%v", d)
	}
}

func TestLastUserLine(t *testing.T) {
	cases := map[string]string{
		"hello":                      "hello",
		"\n\n  first line  \nsecond": "first line",
		"":                           "(empty message)",
		"<user>\na\n</user>\n<user name=\"x\">\nb\n</user>\nWrite the reply.": "b",
		strings.Repeat("x", 200): strings.Repeat("x", 120),
	}
	for in, want := range cases {
		if got := lastUserLine(in); got != want {
			t.Errorf("lastUserLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolResultHelpers(t *testing.T) {
	transcript := "Earlier calls appear as <tool_call> and <tool_result> elements.\n\n<user>\nlog\n</user>\n\n" +
		"<tool_result tool_call_id=\"c1\" name=\"log_food\">\n{\"ok\":true}\n</tool_result>\n\nContinue."
	if !endsWithToolResults(transcript) {
		t.Fatal("should end with tool results")
	}
	if got := firstToolResult(transcript); got != `{"ok":true}` {
		t.Fatalf("first result %q", got)
	}
	if endsWithToolResults(transcript + "\n<user>\nnext\n</user>") {
		t.Fatal("a later user turn means the results were already answered")
	}
	if firstToolResult("no results") != "" || endsWithToolResults("no results") {
		t.Fatal("no results")
	}
}

func TestChunk(t *testing.T) {
	parts := chunk("one two three four five six seven eight nine")
	if strings.Join(parts, "") != "one two three four five six seven eight nine" || len(parts) != 3 {
		t.Fatalf("%q", parts)
	}
	if got := chunk(""); len(got) != 1 || got[0] != "" {
		t.Fatalf("%q", got)
	}
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
