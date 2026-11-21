package helperproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// ErrLineTooLong is returned for a control line or stream header over MaxLine.
var ErrLineTooLong = errors.New("helperproto: line over 64 KiB")

// BreakError marks a protocol break: bad framing, an over-long line, a reply
// to an unknown id, a stream nobody may open. The host kills and restarts a
// helper whose session ended with one; agents keep running.
type BreakError struct{ Reason string }

func (e *BreakError) Error() string { return "helper protocol break: " + e.Reason }

func protocolBreak(format string, args ...any) *BreakError {
	return &BreakError{Reason: fmt.Sprintf(format, args...)}
}

// IsBreak reports whether err is (or wraps) a protocol break.
func IsBreak(err error) bool {
	var b *BreakError
	return errors.As(err, &b)
}

// Error is a control reply's error.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Errorf builds a control error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ErrorCode returns err's control code, or "" when err is not a control error.
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Request is one control request.
type Request struct {
	ID   uint64          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Reply is one control reply.
type Reply struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// StreamHeader is a stream's first line. Only the fields of its kind are set.
type StreamHeader struct {
	S      string          `json:"s"`
	Agent  string          `json:"agent,omitempty"`  // agent
	Kind   string          `json:"kind,omitempty"`   // fwd
	Pane   string          `json:"pane,omitempty"`   // fwd
	Attest json.RawMessage `json:"attest,omitempty"` // fwd
	Root   string          `json:"root,omitempty"`   // scan
	Since  string          `json:"since,omitempty"`  // scan

	// scan (M4)
	Op         string        `json:"op,omitempty"`
	After      string        `json:"after,omitempty"`
	MaxFiles   int           `json:"maxFiles,omitempty"`
	Files      []ScanFileRef `json:"files,omitempty"`
	MaxRecords int           `json:"maxRecords,omitempty"`

	// the header line as received; handlers may re-decode it strictly
	Raw []byte `json:"-"`

	// DecodeErr is set when the line is a JSON object whose "s" is a
	// non-empty string but whose other fields don't fit their types. Only S
	// is set then (no Raw), so a handler for that kind can answer bad_args
	// instead of the whole session breaking, and uses nothing else from the
	// line. Control and agent streams with a DecodeErr are refused as before.
	DecodeErr error `json:"-"`
}

// lineReader reads newline-terminated lines of at most MaxLine bytes.
type lineReader struct{ br *bufio.Reader }

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{br: bufio.NewReaderSize(r, MaxLine+1)}
}

// next returns the next line without its newline. The slice is valid until
// the following call. A line over MaxLine returns ErrLineTooLong; a final
// line without a newline returns io.ErrUnexpectedEOF.
func (l *lineReader) next() ([]byte, error) {
	line, err := l.br.ReadSlice('\n')
	switch {
	case err == nil:
		return line[:len(line)-1], nil
	case errors.Is(err, bufio.ErrBufferFull):
		return nil, ErrLineTooLong
	case errors.Is(err, io.EOF) && len(line) > 0:
		return nil, io.ErrUnexpectedEOF
	default:
		return nil, err
	}
}

// lineWriter writes one JSON value per line; safe for concurrent use.
type lineWriter struct {
	lock chan struct{} // a one-slot semaphore, so waiting can be canceled
	w    io.Writer
}

func newLineWriter(w io.Writer) *lineWriter {
	return &lineWriter{lock: make(chan struct{}, 1), w: w}
}

func (l *lineWriter) write(v any) error { return l.writeCtx(context.Background(), v) }

// writeCtx writes v unless ctx ends first, while waiting for the writer or
// during the write itself (through a write deadline, when the writer has
// one). Nothing is sent once ctx has ended. A write cut short leaves a
// partial line, so the caller must end the session on any error from the
// write itself; ErrLineTooLong and ctx errors before writing are clean.
func (l *lineWriter) writeCtx(ctx context.Context, v any) error {
	b, err := marshalLine(v)
	if err != nil {
		return err
	}
	select {
	case l.lock <- struct{}{}:
	case <-ctx.Done():
		return errNotSent{ctx.Err()}
	}
	defer func() { <-l.lock }()
	if err := ctx.Err(); err != nil {
		return errNotSent{err}
	}
	deadline, _ := ctx.Deadline()
	if d, ok := l.w.(interface{ SetWriteDeadline(time.Time) error }); ok && !deadline.IsZero() {
		_ = d.SetWriteDeadline(deadline)
		defer d.SetWriteDeadline(time.Time{})
	}
	_, err = l.w.Write(b)
	return err
}

func marshalLine(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxLine {
		return nil, ErrLineTooLong
	}
	return append(b, '\n'), nil
}

// errNotSent wraps a context error from before anything was written.
type errNotSent struct{ err error }

func (e errNotSent) Error() string { return e.err.Error() }
func (e errNotSent) Unwrap() error { return e.err }

// WriteHeader writes a stream's first line.
func WriteHeader(w io.Writer, h StreamHeader) error {
	b, err := marshalLine(h)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadHeader reads a stream's first line one byte at a time, so nothing past
// the newline is consumed: the raw bytes after it belong to the stream's user.
func ReadHeader(r io.Reader) (StreamHeader, error) {
	var h StreamHeader
	var buf bytes.Buffer
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		if n == 1 {
			if one[0] == '\n' {
				break
			}
			if buf.Len() >= MaxLine {
				return h, ErrLineTooLong
			}
			buf.WriteByte(one[0])
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && buf.Len() > 0 {
				err = io.ErrUnexpectedEOF
			}
			return h, err
		}
	}
	if err := json.Unmarshal(buf.Bytes(), &h); err != nil {
		// A kind we can still name goes to its handler with the error;
		// anything else (bad JSON, no string "s") stays a stream error.
		var probe struct {
			S *string `json:"s"`
		}
		if json.Unmarshal(buf.Bytes(), &probe) != nil || probe.S == nil || *probe.S == "" {
			return StreamHeader{}, fmt.Errorf("stream header: %w", err)
		}
		// The raw line is dropped: nothing from it but the kind may be used
		// or forwarded. The error names fields and types, never values.
		return StreamHeader{S: *probe.S, DecodeErr: headerTypeError(err)}, nil
	}
	h.Raw = append([]byte(nil), buf.Bytes()...)
	if h.S == "" {
		return h, errors.New("stream header: no kind")
	}
	return h, nil
}

// headerTypeError names the mistyped field and nothing else: Go's own error
// quotes the value for some types (an overflowing number, say).
func headerTypeError(err error) error {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && validHeaderField.MatchString(te.Field) {
		return fmt.Errorf("stream header: %s has the wrong type", te.Field)
	}
	return errors.New("stream header: a field has the wrong type")
}

// validHeaderField is a JSON field path from StreamHeader's own names.
var validHeaderField = regexp.MustCompile(`^[A-Za-z]{1,32}(\.[A-Za-z]{1,32}){0,3}$`)

// decodeArgs decodes a request's args, treating absent args as {}.
// Unknown fields are ignored: later additive fields must not break v1.
func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage("{}")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return Errorf(CodeBadArgs, "args: %v", err)
	}
	return nil
}
