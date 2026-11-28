package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLogFormatJSONLineParses(t *testing.T) {
	var buf bytes.Buffer
	log, err := newLogger(&buf, "json")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello", "agent", "a1")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", buf.String(), err)
	}
	if m["msg"] != "hello" || m["agent"] != "a1" || m["level"] != "INFO" {
		t.Fatalf("unexpected fields: %v", m)
	}
}

func TestLogFormatTextIsDefault(t *testing.T) {
	for _, f := range []string{"", "text"} {
		var buf bytes.Buffer
		log, err := newLogger(&buf, f)
		if err != nil {
			t.Fatal(err)
		}
		log.Info("hello")
		if !strings.Contains(buf.String(), "msg=hello") {
			t.Fatalf("format %q: not text: %q", f, buf.String())
		}
	}
}

func TestLogFormatUnknownRefused(t *testing.T) {
	if _, err := newLogger(&bytes.Buffer{}, "yaml"); err == nil {
		t.Fatal("want error for unknown format")
	}
}
