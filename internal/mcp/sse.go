package mcp

import (
	"bufio"
	"bytes"
	"io"
)

// Event is one SSE event. Raw is its exact bytes including the blank line that
// ends it, so an event that passes the screen is forwarded unchanged.
type Event struct {
	Raw  []byte
	Data []byte // the joined data: lines; empty for comments and keep-alives
}

// ReadEvents calls fn for each SSE event until the stream ends, fn returns
// false, or an error occurs. Lines may end in \n, \r\n or \r (SSE allows all
// three); only \n and \r\n are handled, which is what MCP servers send.
func ReadEvents(r io.Reader, fn func(Event) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var raw, data bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			raw.Write(line)
			t := bytes.TrimRight(line, "\r\n")
			switch {
			case len(t) == 0: // blank line: dispatch
				ev := Event{Raw: append([]byte(nil), raw.Bytes()...), Data: append([]byte(nil), data.Bytes()...)}
				raw.Reset()
				data.Reset()
				if !fn(ev) {
					return nil
				}
			case bytes.HasPrefix(t, []byte("data:")):
				d := bytes.TrimPrefix(t, []byte("data:"))
				d = bytes.TrimPrefix(d, []byte(" "))
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(d)
			}
		}
		if err != nil {
			if raw.Len() > 0 { // stream ended mid-event: hand over what there is
				fn(Event{Raw: append([]byte(nil), raw.Bytes()...), Data: append([]byte(nil), data.Bytes()...)})
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// WriteEvent renders a data-only SSE event.
func WriteEvent(w io.Writer, data []byte) error {
	_, err := w.Write(append(append([]byte("event: message\ndata: "), data...), '\n', '\n'))
	return err
}
