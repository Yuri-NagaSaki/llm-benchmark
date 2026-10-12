package bench

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestReadSSEFieldsAndEOF(t *testing.T) {
	input := "\ufeff: heartbeat\r\nid: 42\r\nevent: response.output_text.delta\r\ndata: {\r\ndata:  \"delta\": \"hi\"}\r\n\r\n: ping\n\nretry: 1000\ndata: last"
	var frames []SSEFrame
	err := ReadSSE(strings.NewReader(input), func(frame SSEFrame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []SSEFrame{
		{Event: "response.output_text.delta", ID: "42", Data: "{\n \"delta\": \"hi\"}"},
		{ID: "42", Data: "last"},
	}
	if !reflect.DeepEqual(frames, want) {
		t.Fatalf("frames = %#v, want %#v", frames, want)
	}
}

func TestReadSSEEmptyFramesAndIDReset(t *testing.T) {
	var frames []SSEFrame
	err := ReadSSE(strings.NewReader("\n\n: ping\n\nid: old\n\ndata:\n\nid:\ndata:  leading\nid: ignored\x00id\n\n"), func(frame SSEFrame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []SSEFrame{{ID: "old"}, {Data: " leading"}}
	if !reflect.DeepEqual(frames, want) {
		t.Fatalf("frames = %#v", frames)
	}
}

func TestReadSSEBoundsWholeFrameAndIndividualLines(t *testing.T) {
	for name, input := range map[string]string{
		"long line":        "data: " + strings.Repeat("x", MaxSSEFrameBytes),
		"many data fields": strings.Repeat("data: small\n", MaxSSEFrameBytes/10),
		"comment flood":    strings.Repeat(": ping\n", MaxSSEFrameBytes/6),
	} {
		t.Run(name, func(t *testing.T) {
			err := ReadSSE(strings.NewReader(input), func(SSEFrame) error {
				t.Fatal("oversized frame emitted")
				return nil
			})
			if !errors.Is(err, ErrSSEFrameTooLarge) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	data := strings.Repeat("x", 128<<10)
	err := ReadSSE(strings.NewReader("data: "+data+"\n\n"), func(frame SSEFrame) error {
		if frame.Data != data {
			t.Fatal("valid large frame was truncated")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestReadSSEPropagatesCallbackAndReaderErrors(t *testing.T) {
	stop := errors.New("stop consuming")
	if err := ReadSSE(strings.NewReader("data: 1\n\ndata: 2\n\n"), func(SSEFrame) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error = %v", err)
	}
	if err := ReadSSE(failingSSEReader{}, func(SSEFrame) error { return nil }); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("reader error = %v", err)
	}
}

type failingSSEReader struct{}

func (failingSSEReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
