package bench

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// MaxSSEFrameBytes limits both an individual line and the complete buffered frame.
const MaxSSEFrameBytes = 1 << 20

var ErrSSEFrameTooLarge = errors.New("SSE frame exceeds 1 MiB")

type SSEFrame struct {
	Event string
	Data  string
	ID    string
}

// ReadSSE accepts LF and CRLF, joins multiple data fields with newlines, and
// dispatches a final frame at EOF. EOF alone never implies protocol completion.
func ReadSSE(r io.Reader, emit func(SSEFrame) error) error {
	reader := bufio.NewReaderSize(r, 32<<10)
	var frame SSEFrame
	var data []string
	var line []byte
	frameBytes := 0
	firstLine := true
	dispatch := func() error {
		if len(data) != 0 {
			frame.Data = strings.Join(data, "\n")
			if err := emit(frame); err != nil {
				return err
			}
		}
		// Event names are per-frame; the last event ID persists, as in SSE.
		frame.Event, frame.Data = "", ""
		data = nil
		frameBytes = 0
		return nil
	}
	for {
		fragment, err := reader.ReadSlice('\n')
		frameBytes += len(fragment)
		if frameBytes > MaxSSEFrameBytes {
			return ErrSSEFrameTooLarge
		}
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(line) != 0 {
			s := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
			line = line[:0]
			if firstLine {
				s = strings.TrimPrefix(s, "\ufeff")
				firstLine = false
			}
			if s == "" {
				if emitErr := dispatch(); emitErr != nil {
					return emitErr
				}
			} else if !strings.HasPrefix(s, ":") {
				field, value, _ := strings.Cut(s, ":")
				value = strings.TrimPrefix(value, " ")
				switch field {
				case "data":
					data = append(data, value)
				case "event":
					frame.Event = value
				case "id":
					if !strings.ContainsRune(value, '\x00') {
						frame.ID = value
					}
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return dispatch()
		}
	}
}
