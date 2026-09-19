// Package log provides ANSI colorizing for the single-line JSON records that
// the Tinkerbell binaries emit through log/slog.
//
// The colorizer is an io.Writer wrapper rather than an slog.Handler wrapper, so
// the JSON itself is still whatever slog.JSONHandler produced: bytes are only
// ever passed through, never re-encoded, with escape sequences injected between
// them. Stripping the escape sequences from the output yields the original line
// byte for byte.
package log

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Colour mode names, as accepted by Mode.Set.
const (
	ModeAuto   = "auto"
	ModeAlways = "always"
	ModeNever  = "never"
)

// Mode is the --log-color flag value: whether to colorize log output.
//
// The zero value is ModeAuto, so a Mode is usable without construction.
type Mode struct {
	value string
}

// String implements flag.Value.
func (m *Mode) String() string {
	if m == nil || m.value == "" {
		return ModeAuto
	}

	return m.value
}

// Set implements flag.Value.
func (m *Mode) Set(s string) error {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case ModeAuto, "":
		m.value = ModeAuto
	case ModeAlways, "true", "1", "yes", "force":
		m.value = ModeAlways
	case ModeNever, "false", "0", "no", "none":
		m.value = ModeNever
	default:
		return fmt.Errorf("invalid log color mode %q, must be one of %s, %s, %s", s, ModeAuto, ModeAlways, ModeNever)
	}

	return nil
}

// Enabled reports whether output written to w should be colorized.
//
// An explicit "always" or "never" wins outright. Otherwise ("auto"):
//
//   - FORCE_COLOR, set to anything but 0/false/no/off, turns colour on. It
//     deliberately outranks NO_COLOR: it is the more specific, more deliberate
//     override, which is also how chalk and the tools that copied it behave.
//   - NO_COLOR, set to any non-empty value, turns colour off (https://no-color.org).
//   - Otherwise colour is on only when w is a terminal.
func (m *Mode) Enabled(w io.Writer) bool {
	switch m.String() {
	case ModeAlways:
		return true
	case ModeNever:
		return false
	}

	if v, ok := os.LookupEnv("FORCE_COLOR"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "0", "false", "no", "off":
			return false
		default:
			return true
		}
	}

	// https://no-color.org: "when present and not an empty string (regardless
	// of its value)".
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}

	f, ok := w.(*os.File)

	return ok && term.IsTerminal(int(f.Fd()))
}

// ANSI escape sequences. Punctuation uses the dim variant of the level's colour
// so the structure recedes and the content stands out.
const (
	ansiReset = "\x1b[0m"

	colorKey     = "\x1b[36m"   // cyan
	colorString  = "\x1b[32m"   // green
	colorNumber  = "\x1b[33m"   // yellow
	colorLiteral = "\x1b[35m"   // magenta: true, false, null
	colorDefault = "\x1b[97m"   // bright white
	colorDim     = "\x1b[2;37m" // dim grey
)

// levelColors maps an slog level to the line's accent colour and the dimmed
// variant used for punctuation. logr maps Error to slog level 8 and V(n).Info
// to -n, and the loggers' ReplaceAttr rewrites the level to that integer.
func levelColors(level int) (accent, dim string) {
	switch {
	case level >= 8: // error
		return "\x1b[91m", "\x1b[2;31m"
	case level >= 4: // warn
		return "\x1b[93m", "\x1b[2;33m"
	case level >= 0: // info
		return colorDefault, colorDim
	default: // debug and the more verbose V-levels
		return "\x1b[90m", "\x1b[2;90m"
	}
}

// levelInfo is the level assumed for a record with no usable "level" field.
const levelInfo = 0

// NewColorWriter returns an io.Writer that colorizes each complete JSON line
// written to it and forwards the result to w. Lines that are not valid JSON are
// passed through untouched.
//
// Writes are buffered to newline boundaries, so a record split across several
// Write calls is still colorized as one line. slog.JSONHandler in fact emits
// exactly one newline-terminated Write per record, under its own mutex, but the
// buffering keeps this correct for anything else sharing the stream.
func NewColorWriter(w io.Writer) io.Writer {
	return &colorWriter{w: w}
}

type colorWriter struct {
	w io.Writer

	mu  sync.Mutex
	buf []byte // bytes of an as-yet-unterminated line
	out []byte // scratch for the colorized line, reused across writes
}

// Write implements io.Writer. All of p is always consumed into the internal
// buffer, so the returned count is len(p) even when forwarding fails.
func (c *colorWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buf = append(c.buf, p...)

	consumed := 0
	for {
		i := bytes.IndexByte(c.buf[consumed:], '\n')
		if i < 0 {
			break
		}
		line := c.buf[consumed : consumed+i]
		consumed += i + 1

		// Into scratch, not onto line: Colorize can return its argument, which
		// aliases c.buf, and appending to that would write into the buffer.
		c.out = append(appendColorized(c.out[:0], line), '\n')
		if _, err := c.w.Write(c.out); err != nil {
			c.buf = append(c.buf[:0], c.buf[consumed:]...)

			return len(p), err
		}
	}
	// Move any partial line to the front so the buffer does not grow without
	// bound. append to a zero-length prefix of the same array copies in place.
	c.buf = append(c.buf[:0], c.buf[consumed:]...)

	return len(p), nil
}

// Colorize returns line with ANSI escape sequences injected around its JSON
// tokens. line must not contain a newline. Input that is not valid JSON is
// returned unchanged, so the result may alias line.
func Colorize(line []byte) []byte {
	if !json.Valid(line) {
		return line
	}

	return appendColorized(nil, line)
}

// appendColorized appends the colorized form of line to dst and returns the
// extended slice. line is assumed to be valid JSON; callers that cannot
// guarantee that should go through Colorize.
func appendColorized(dst, line []byte) []byte {
	if !json.Valid(line) {
		return append(dst, line...)
	}

	toks := tokenize(line)
	accent, dim := levelColors(levelOf(line, toks))

	out := dst
	lastKey := ""
	for _, t := range toks {
		seg := line[t.start:t.end]

		var color string
		switch t.kind {
		case tokSpace:
			out = append(out, seg...)

			continue
		case tokPunct:
			if seg[0] == ',' || seg[0] == '{' || seg[0] == '[' {
				lastKey = ""
			}
			color = dim
		case tokKey:
			lastKey = string(bytes.Trim(seg, `"`))
			color = colorKey
		case tokString:
			// The level and the message are what the eye looks for first, so
			// they carry the line's accent colour rather than the string colour.
			if lastKey == "level" || lastKey == "msg" {
				color = accent
			} else {
				color = colorString
			}
		case tokNumber:
			// The agent's logger leaves the level as a JSON number where
			// tinkerbell's rewrites it to a string; accent both, so the two
			// binaries read the same.
			if lastKey == "level" {
				color = accent
			} else {
				color = colorNumber
			}
		case tokLiteral:
			color = colorLiteral
		}

		out = append(out, color...)
		out = append(out, seg...)
		out = append(out, ansiReset...)
	}

	return out
}

type tokenKind int

const (
	tokSpace tokenKind = iota
	tokPunct
	tokKey
	tokString
	tokNumber
	tokLiteral
)

type token struct {
	kind  tokenKind
	start int
	end   int
	depth int
}

// tokenize splits valid JSON into spans of the original bytes. Every byte of b
// belongs to exactly one token, which is what lets Colorize reproduce the input
// exactly.
func tokenize(b []byte) []token {
	toks := make([]token, 0, 64)
	depth := 0

	for i := 0; i < len(b); {
		switch c := b[i]; c {
		case ' ', '\t', '\r':
			j := i
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\r') {
				j++
			}
			toks = append(toks, token{tokSpace, i, j, depth})
			i = j

		case '{', '[':
			depth++
			toks = append(toks, token{tokPunct, i, i + 1, depth})
			i++

		case '}', ']':
			toks = append(toks, token{tokPunct, i, i + 1, depth})
			depth--
			i++

		case ':', ',':
			toks = append(toks, token{tokPunct, i, i + 1, depth})
			i++

		case '"':
			j := i + 1
			for j < len(b) {
				if b[j] == '\\' {
					j += 2

					continue
				}
				if b[j] == '"' {
					j++

					break
				}
				j++
			}
			if j > len(b) {
				j = len(b)
			}
			// A string is a key when the next non-space byte is a colon.
			k := j
			for k < len(b) && (b[k] == ' ' || b[k] == '\t') {
				k++
			}
			kind := tokString
			if k < len(b) && b[k] == ':' {
				kind = tokKey
			}
			toks = append(toks, token{kind, i, j, depth})
			i = j

		default:
			j := i
			for j < len(b) && !isStructural(b[j]) {
				j++
			}
			if j == i {
				j++ // never stall, whatever the input
			}
			kind := tokLiteral
			if c == '-' || (c >= '0' && c <= '9') {
				kind = tokNumber
			}
			toks = append(toks, token{kind, i, j, depth})
			i = j
		}
	}

	return toks
}

func isStructural(c byte) bool {
	switch c {
	case '{', '}', '[', ']', ':', ',', '"', ' ', '\t', '\r':
		return true
	}

	return false
}

// levelOf returns the slog level from the record's top-level "level" field, or
// levelInfo when there is none it can read.
func levelOf(b []byte, toks []token) int {
	for i, t := range toks {
		if t.kind != tokKey || t.depth != 1 || string(b[t.start:t.end]) != `"level"` {
			continue
		}
		for _, v := range toks[i+1:] {
			switch v.kind {
			case tokSpace:
				continue
			case tokPunct:
				if b[v.start] == ':' {
					continue
				}

				return levelInfo
			case tokString, tokNumber:
				return parseLevel(string(bytes.Trim(b[v.start:v.end], `"`)))
			default:
				return levelInfo
			}
		}
	}

	return levelInfo
}

// parseLevel reads the integer level the loggers' ReplaceAttr writes, falling
// back to slog's level names for output produced without it.
func parseLevel(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	switch strings.ToUpper(s) {
	case "ERROR":
		return 8
	case "WARN", "WARNING":
		return 4
	case "INFO":
		return 0
	case "DEBUG":
		return -4
	}

	return levelInfo
}
