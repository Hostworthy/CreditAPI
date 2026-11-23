package fakecore

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
	d := Directives("please [fake:slow] and [fake:echo-input] [fake:Bad]")
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
		if got := LastUserLine(in); got != want {
			t.Errorf("LastUserLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolResultHelpers(t *testing.T) {
	transcript := "Earlier calls appear as <tool_call> and <tool_result> elements.\n\n<user>\nlog\n</user>\n\n" +
		"<tool_result tool_call_id=\"c1\" name=\"log_food\">\n{\"ok\":true}\n</tool_result>\n\nContinue."
	if !EndsWithToolResults(transcript) {
		t.Fatal("should end with tool results")
	}
	if got := FirstToolResult(transcript); got != `{"ok":true}` {
		t.Fatalf("first result %q", got)
	}
	if EndsWithToolResults(transcript + "\n<user>\nnext\n</user>") {
		t.Fatal("a later user turn means the results were already answered")
	}
	if FirstToolResult("no results") != "" || EndsWithToolResults("no results") {
		t.Fatal("no results")
	}
}

func TestChunk(t *testing.T) {
	parts := Chunk("one two three four five six seven eight nine")
	if strings.Join(parts, "") != "one two three four five six seven eight nine" || len(parts) != 3 {
		t.Fatalf("%q", parts)
	}
	if got := Chunk(""); len(got) != 1 || got[0] != "" {
		t.Fatalf("%q", got)
	}
}

func TestImageSizeAndDescribe(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 5))); err != nil {
		t.Fatal(err)
	}
	if got := ImageSize(buf.Bytes()); got != "4x5" {
		t.Fatalf("size %q", got)
	}
	if got := ImageSize([]byte("nope")); got != "unreadable" {
		t.Fatalf("size %q", got)
	}
	if DescribeImages(nil) != "" {
		t.Fatal("no images")
	}
	if got := DescribeImages([]string{"image/png 4x5"}); got != "I see 1 image(s): image/png 4x5." {
		t.Fatalf("%q", got)
	}
}

func TestWhoami(t *testing.T) {
	t.Setenv("HOME", filepath.Join(string(os.PathSeparator)+"data", "subscriptions", "abc123", "home"))
	if got := Whoami("cursor"); got != "Served by cursor subscription abc123" {
		t.Fatalf("%q", got)
	}
}

func TestChooseTools(t *testing.T) {
	tools := []Tool{{Name: "get_weather"}, {Name: "log_food"}}
	if got := ChooseTools(tools, "please log_food"); len(got) != 1 || got[0].Name != "log_food" {
		t.Fatalf("%v", got)
	}
	if got := ChooseTools(tools, "anything"); len(got) != 1 || got[0].Name != "get_weather" {
		t.Fatalf("%v", got)
	}
	if got := ChooseTools(nil, "x"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	if ToolResults("<tool_result>a</tool_result><tool_result>b</tool_result>") != 2 {
		t.Fatal("count")
	}
	if !strings.HasPrefix(LongReply(), "All work") || len(LongReply()) < 4000 {
		t.Fatal("long reply")
	}
}
