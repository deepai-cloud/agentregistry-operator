package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const catalogResponseLimit = 1 << 20

type catalogResponseKey struct{}

func filterCatalogResponse(r *http.Request) bool {
	// GET can replay a discovery response after an SSE disconnect. Inspect the
	// result shape as well as POST discovery requests so replay stays filtered.
	return r.Method == http.MethodGet || r.Context().Value(catalogResponseKey{}) == true
}

func modifyCatalogResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || !filterCatalogResponse(resp.Request) {
		return nil
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return errors.New("compressed MCP discovery response")
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return err
	}
	switch mediaType {
	case "application/json":
		data, err := io.ReadAll(io.LimitReader(resp.Body, catalogResponseLimit+1))
		if err != nil {
			return err
		}
		if len(data) > catalogResponseLimit {
			return errors.New("MCP discovery response too large")
		}
		data, err = filterCatalogMessage(data)
		if err != nil {
			return err
		}
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(data))
		resp.ContentLength = int64(len(data))
		resp.Header.Set("Content-Length", strconv.Itoa(len(data)))
	case "text/event-stream":
		resp.Body = &catalogEventStream{ReadCloser: resp.Body, reader: bufio.NewReader(resp.Body), firstLine: true, trailers: &resp.Trailer}
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
	default:
		return errors.New("unsupported MCP discovery response type")
	}
	// These describe the upstream bytes, which may have changed.
	for _, name := range []string{"ETag", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest", "Trailer"} {
		resp.Header.Del(name)
	}
	resp.Trailer = nil
	return nil
}

// RawMessage keeps request IDs, schemas, metadata and pagination cursors intact,
// including integers that cannot round-trip through float64. Result shapes also
// let us recognize replayed discovery without keeping per-session request state.
func filterCatalogMessage(data []byte) ([]byte, error) {
	var rpc map[string]json.RawMessage
	if err := json.Unmarshal(data, &rpc); err != nil || rpc == nil {
		return nil, errors.New("invalid MCP discovery message")
	}
	if _, ok := rpc["error"]; ok || rpc["method"] != nil || rpc["result"] == nil {
		return data, nil
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(rpc["result"], &result); err != nil || result == nil {
		return nil, errors.New("invalid MCP discovery result")
	}
	changed := false
	for field, allowed := range map[string]map[string]bool{"tools": catalogTools, "prompts": catalogPrompts} {
		raw, ok := result[field]
		if !ok {
			continue
		}
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil || entries == nil {
			return nil, errors.New("invalid MCP discovery list")
		}
		kept := make([]json.RawMessage, 0, len(entries))
		for _, entry := range entries {
			var item map[string]json.RawMessage
			if err := json.Unmarshal(entry, &item); err != nil {
				return nil, err
			}
			var name string
			if json.Unmarshal(item["name"], &name) == nil && allowed[name] {
				kept = append(kept, entry)
			}
		}
		result[field], _ = json.Marshal(kept)
		changed = true
	}
	// Both pinned upstream versions use this exact static overview. Adapt only
	// that text, including on replay; user-supplied prompt/tool content is untouched.
	const overview = "Give me an overview of what's available in the agent registry. " +
		"Use list_servers, list_agents, and list_skills to see what's published. " +
		"Also check list_deployments to see what's currently deployed. " +
		"Summarize the results in a clear table format showing name, description, and latest version for each resource type."
	var messages []map[string]json.RawMessage
	var description string
	if json.Unmarshal(result["description"], &description) == nil && description == "Overview of registry contents" && json.Unmarshal(result["messages"], &messages) == nil {
		for _, message := range messages {
			var content map[string]json.RawMessage
			var text string
			if json.Unmarshal(message["content"], &content) == nil && json.Unmarshal(content["text"], &text) == nil && text == overview {
				content["text"], _ = json.Marshal(strings.Replace(overview, "Also check list_deployments to see what's currently deployed. ", "", 1))
				message["content"], _ = json.Marshal(content)
				result["messages"], _ = json.Marshal(messages)
				changed = true
			}
		}
	}
	if !changed {
		return data, nil
	}
	rpc["result"], _ = json.Marshal(result)
	return json.Marshal(rpc)
}

// Transform one bounded SSE event at a time. Reading and closing the wrapper
// directly controls the upstream body; no goroutine or whole-stream buffer is
// needed, and complete events are forwarded while the stream remains open.
type catalogEventStream struct {
	io.ReadCloser
	reader            *bufio.Reader
	pending           []byte
	firstLine, skipLF bool
	trailers          *http.Header
}

func (s *catalogEventStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(s.pending) == 0 {
		frame, err := s.readEvent()
		if s.trailers != nil {
			// The transport may populate trailers while reading the final event.
			// They describe upstream bytes, just like the removed header digests.
			*s.trailers = nil
		}
		if err != nil {
			return 0, err
		}
		s.pending = frame
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *catalogEventStream) readEvent() ([]byte, error) {
	var frame, line, data []byte
	event := ""
	for size := 0; size < catalogResponseLimit; size++ {
		b, err := s.reader.ReadByte()
		if err != nil {
			if err == io.EOF && (len(frame) > 0 || len(line) > 0) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if s.skipLF {
			s.skipLF = false
			if b == '\n' {
				continue
			}
		}
		if b != '\r' && b != '\n' {
			line = append(line, b)
			continue
		}
		s.skipLF = b == '\r'
		if s.firstLine {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			s.firstLine = false
		}
		if len(line) == 0 {
			if len(data) > 1 && (event == "" || event == "message") {
				filtered, err := filterCatalogMessage(data[:len(data)-1])
				if err != nil {
					return nil, err
				}
				// Keep comments, event IDs and retry fields, replacing only data.
				var out []byte
				for _, field := range bytes.Split(frame, []byte("\n")) {
					if len(field) > 0 && !bytes.Equal(field, []byte("data")) && !bytes.HasPrefix(field, []byte("data:")) {
						out = append(out, field...)
						out = append(out, '\n')
					}
				}
				for _, part := range bytes.Split(filtered, []byte("\n")) {
					out = append(out, "data: "...)
					out = append(out, part...)
					out = append(out, '\n')
				}
				frame = out
			}
			return append(frame, '\n'), nil
		}
		frame = append(frame, line...)
		frame = append(frame, '\n')
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			data = append(data, value...)
			data = append(data, '\n')
		case "event":
			event = string(value)
		}
		line = line[:0]
	}
	return nil, errors.New("MCP discovery event too large")
}
