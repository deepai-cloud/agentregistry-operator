package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
)

// Read only through the readiness request's response. An SSE stream may contain
// notifications first and need not close immediately after initialization.
func readMCPInitialization(resp *http.Response) ([]byte, error) {
	const limit = 64 << 10
	contentTypes := resp.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return nil, errors.New("MCP initialization requires a single Content-Type")
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil {
		return nil, err
	}
	if mediaType == "application/json" {
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if len(data) > limit {
			return nil, errors.New("MCP initialization response too large")
		}
		return data, err
	}
	if mediaType != "text/event-stream" {
		return nil, errors.New("unsupported MCP initialization response type")
	}
	reader := bufio.NewReader(io.LimitReader(resp.Body, limit))
	var line, data []byte
	event := ""
	firstLine, skipLF := true, false
	for read := 0; read < limit; read++ {
		b, err := reader.ReadByte()
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if skipLF {
			skipLF = false
			if b == '\n' {
				continue
			}
		}
		if b != '\r' && b != '\n' {
			line = append(line, b)
			continue
		}
		// Dispatch CR-terminated lines immediately, even on a still-open stream.
		// A following LF is part of that same terminator, not a second line.
		skipLF = b == '\r'
		if firstLine {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			firstLine = false
		}
		if len(line) == 0 {
			// Every data field contributes a trailing newline. A single
			// empty field is a control event, not a JSON-RPC message.
			if len(data) > 1 && (event == "" || event == "message") {
				var message struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				if err := json.Unmarshal(data, &message); err != nil {
					return nil, err
				}
				if string(message.ID) == "1" && message.Method == "" {
					return data, nil
				}
			}
			data = data[:0]
			event = ""
		} else {
			field, value, _ := bytes.Cut(line, []byte(":"))
			value = bytes.TrimPrefix(value, []byte(" "))
			switch string(field) {
			case "data":
				data = append(data, value...)
				data = append(data, '\n')
			case "event":
				event = string(value)
			}
		}
		line = line[:0]
	}
	return nil, errors.New("MCP initialization stream too large")
}
