package log

import (
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// ansiSeq matches the escape sequences Colorize injects, so tests can recover
// the bytes that were there before colorizing.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(b []byte) string {
	return string(ansiSeq.ReplaceAll(b, nil))
}

// corpus covers the shapes the JSON loggers actually emit plus the parsing
// corners: escapes, nesting, every scalar type, and levels at each colour band.
var corpus = map[string]string{
	"typical info record":  `{"time":"2026-09-19T16:12:24.723529+02:00","level":"0","caller":"cmd/tinkerbell/cmd.go:197","msg":"starting tinkerbell","logger":"cli","smeeEnabled":true}`,
	"error with escapes":   `{"level":"8","msg":"say \"hi\" \\ there","err":null}`,
	"nested and arrays":    `{"level":"0","msg":"m","nested":{"a":[1,2,"three"],"b":false},"empty":{},"none":[]}`,
	"numbers":              `{"level":"0","msg":"m","int":7080,"neg":-1,"float":1.5,"exp":-1.5e3,"zero":0}`,
	"unicode escapes":      `{"level":"0","msg":"caf\u00e9 \u2014 dash"}`,
	"colon inside string":  `{"level":"0","msg":"a:b","caller":"cmd/tinkerbell/cmd.go:197"}`,
	"braces inside string": `{"level":"0","msg":"{\"not\":\"json\"}"}`,
	"numeric level":        `{"level":8,"msg":"numeric level field"}`,
	"named level":          `{"level":"ERROR","msg":"named level field"}`,
	"no level field":       `{"msg":"no level at all","x":1}`,
	"no msg field":         `{"level":"0","x":1}`,
	"empty object":         `{}`,
	"whitespace":           `{"level": "0", "msg": "spaced out"}`,
	"top-level array":      `[{"level":"0","msg":"one"},{"level":"8","msg":"two"}]`,
}

// The whole point of colorizing at the byte level rather than re-encoding: the
// JSON must survive untouched, so that piping through `sed` to strip escapes
// (or a terminal that ignores them) still yields exactly what slog wrote.
func TestColorizeIsByteFaithful(t *testing.T) {
	for name, line := range corpus {
		t.Run(name, func(t *testing.T) {
			got := stripANSI(Colorize([]byte(line)))
			if diff := cmp.Diff(line, got); diff != "" {
				t.Errorf("stripping ANSI did not recover the input (-want +got):\n%s", diff)
			}
		})
	}
}

func TestColorizeLevelAccent(t *testing.T) {
	tests := map[string]struct {
		line       string
		wantAccent string
		wantDim    string
	}{
		"error is red":      {`{"level":"8","msg":"m"}`, "\x1b[91m", "\x1b[2;31m"},
		"warn is yellow":    {`{"level":"4","msg":"m"}`, "\x1b[93m", "\x1b[2;33m"},
		"info is default":   {`{"level":"0","msg":"m"}`, colorDefault, colorDim},
		"verbose is grey":   {`{"level":"-2","msg":"m"}`, "\x1b[90m", "\x1b[2;90m"},
		"numeric level":     {`{"level":8,"msg":"m"}`, "\x1b[91m", "\x1b[2;31m"},
		"named level":       {`{"level":"ERROR","msg":"m"}`, "\x1b[91m", "\x1b[2;31m"},
		"missing level":     {`{"msg":"m"}`, colorDefault, colorDim},
		"unparseable level": {`{"level":"banana","msg":"m"}`, colorDefault, colorDim},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := string(Colorize([]byte(tt.line)))

			// The message value carries the accent.
			if !strings.Contains(got, tt.wantAccent+`"m"`) {
				t.Errorf("msg is not in the accent colour %q:\n%q", tt.wantAccent, got)
			}
			// So does the level, whether it is a JSON string (tinkerbell's
			// logger) or a JSON number (the agent's).
			if !strings.Contains(got, tt.wantAccent+`"level"`) && strings.Contains(tt.line, `"level"`) {
				lvl := tt.line[strings.Index(tt.line, `"level":`)+len(`"level":`):]
				lvl = lvl[:strings.IndexAny(lvl, ",}")]
				if !strings.Contains(got, tt.wantAccent+lvl) {
					t.Errorf("level value %s is not in the accent colour %q:\n%q", lvl, tt.wantAccent, got)
				}
			}
			// Punctuation carries the dimmed variant.
			if !strings.Contains(got, tt.wantDim+"{") {
				t.Errorf("punctuation is not in the dim colour %q:\n%q", tt.wantDim, got)
			}
		})
	}
}

func TestColorizeTokenRoles(t *testing.T) {
	got := string(Colorize([]byte(`{"level":"0","msg":"m","s":"str","n":7080,"b":true,"z":null}`)))

	for _, want := range []string{
		colorKey + `"level"`,  // keys are cyan
		colorKey + `"s"`,      //
		colorString + `"str"`, // ordinary string values are green
		colorNumber + "7080",  // numbers are yellow
		colorLiteral + "true", // literals are magenta
		colorLiteral + "null", //
		colorDefault + `"m"`,  // msg takes the level accent
		colorDefault + `"0"`,  // and so does level
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%q", want, got)
		}
	}
}

func TestColorizePassesThroughNonJSON(t *testing.T) {
	for name, line := range map[string]string{
		"plain text":    "W0919 16:15:06.397735 options.go:369] No CIDR specified",
		"truncated":     `{"level":"0","msg":`,
		"empty":         "",
		"bare word":     "null-ish",
		"trailing gunk": `{"level":"0"} trailing`,
	} {
		t.Run(name, func(t *testing.T) {
			got := string(Colorize([]byte(line)))
			if got != line {
				t.Errorf("non-JSON was modified:\n want %q\n  got %q", line, got)
			}
		})
	}
}

func TestColorWriter(t *testing.T) {
	t.Run("colorizes one line per record", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewColorWriter(&buf)

		line := `{"level":"0","msg":"one"}`
		if _, err := w.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}

		out := buf.String()
		if !strings.HasSuffix(out, "\n") {
			t.Errorf("output lost its newline: %q", out)
		}
		if got := stripANSI([]byte(out)); got != line+"\n" {
			t.Errorf("want %q, got %q", line+"\n", got)
		}
	})

	t.Run("a record split across writes is still colorized whole", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewColorWriter(&buf)

		line := `{"level":"8","msg":"split"}`
		for _, part := range []string{line[:10], line[10:], "\n"} {
			if _, err := w.Write([]byte(part)); err != nil {
				t.Fatal(err)
			}
		}

		if got := stripANSI(buf.Bytes()); got != line+"\n" {
			t.Errorf("want %q, got %q", line+"\n", got)
		}
		// Whole-line colorizing means the error accent was applied, which a
		// per-Write implementation could not have managed.
		if !strings.Contains(buf.String(), "\x1b[91m") {
			t.Errorf("error accent missing from reassembled line: %q", buf.String())
		}
	})

	t.Run("holds an unterminated line until its newline arrives", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewColorWriter(&buf)

		if _, err := w.Write([]byte(`{"level":"0","msg":"pending"}`)); err != nil {
			t.Fatal(err)
		}
		if buf.Len() != 0 {
			t.Errorf("wrote a partial line early: %q", buf.String())
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			t.Fatal(err)
		}
		if buf.Len() == 0 {
			t.Error("nothing written after the newline arrived")
		}
	})

	t.Run("several records in one write", func(t *testing.T) {
		var buf bytes.Buffer
		w := NewColorWriter(&buf)

		in := `{"level":"0","msg":"a"}` + "\n" + `{"level":"8","msg":"b"}` + "\n"
		if _, err := w.Write([]byte(in)); err != nil {
			t.Fatal(err)
		}
		if got := stripANSI(buf.Bytes()); got != in {
			t.Errorf("want %q, got %q", in, got)
		}
	})

	t.Run("reports the write error and consumes the input", func(t *testing.T) {
		wantErr := errors.New("boom")
		w := NewColorWriter(errWriter{err: wantErr})

		in := []byte(`{"level":"0","msg":"a"}` + "\n")
		n, err := w.Write(in)
		if !errors.Is(err, wantErr) {
			t.Errorf("want %v, got %v", wantErr, err)
		}
		if n != len(in) {
			t.Errorf("want n=%d, got %d", len(in), n)
		}
	})
}

type errWriter struct{ err error }

func (e errWriter) Write([]byte) (int, error) { return 0, e.err }

func TestModeSet(t *testing.T) {
	tests := map[string]struct {
		input   string
		want    string
		wantErr bool
	}{
		"auto":                {"auto", ModeAuto, false},
		"always":              {"always", ModeAlways, false},
		"never":               {"never", ModeNever, false},
		"empty is auto":       {"", ModeAuto, false},
		"true is always":      {"true", ModeAlways, false},
		"false is never":      {"false", ModeNever, false},
		"mixed case":          {"AlWaYs", ModeAlways, false},
		"padded":              {"  never  ", ModeNever, false},
		"rubbish is rejected": {"banana", "", true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var m Mode
			err := m.Set(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error for %q, got none", tt.input)
				}

				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m.String() != tt.want {
				t.Errorf("want %q, got %q", tt.want, m.String())
			}
		})
	}
}

func TestModeZeroValueIsAuto(t *testing.T) {
	var m Mode
	if m.String() != ModeAuto {
		t.Errorf("want %q, got %q", ModeAuto, m.String())
	}
}

// Enabled's precedence is the whole contract of the flag, so pin every rung of
// it, including FORCE_COLOR deliberately outranking NO_COLOR.
func TestModeEnabled(t *testing.T) {
	tests := map[string]struct {
		mode       string
		forceColor *string
		noColor    *string
		want       bool
	}{
		"always wins over NO_COLOR":    {ModeAlways, nil, ptr("1"), true},
		"never wins over FORCE_COLOR":  {ModeNever, ptr("1"), nil, false},
		"auto, no env, not a terminal": {ModeAuto, nil, nil, false},
		"auto with FORCE_COLOR":        {ModeAuto, ptr("1"), nil, true},
		"auto with empty FORCE_COLOR":  {ModeAuto, ptr(""), nil, true},
		"auto with FORCE_COLOR=0":      {ModeAuto, ptr("0"), nil, false},
		"auto with FORCE_COLOR=false":  {ModeAuto, ptr("false"), nil, false},
		"auto with NO_COLOR":           {ModeAuto, nil, ptr("1"), false},
		"auto with empty NO_COLOR":     {ModeAuto, nil, ptr(""), false},
		"FORCE_COLOR beats NO_COLOR":   {ModeAuto, ptr("1"), ptr("1"), true},
		"FORCE_COLOR=0 with NO_COLOR":  {ModeAuto, ptr("0"), ptr("1"), false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			// t.Setenv cannot unset, so clear both and set only what the case wants.
			t.Setenv("FORCE_COLOR", "")
			t.Setenv("NO_COLOR", "")
			os.Unsetenv("FORCE_COLOR")
			os.Unsetenv("NO_COLOR")
			if tt.forceColor != nil {
				t.Setenv("FORCE_COLOR", *tt.forceColor)
			}
			if tt.noColor != nil {
				t.Setenv("NO_COLOR", *tt.noColor)
			}

			var m Mode
			if err := m.Set(tt.mode); err != nil {
				t.Fatal(err)
			}
			// io.Discard is not an *os.File, so it stands in for "not a terminal".
			if got := m.Enabled(io.Discard); got != tt.want {
				t.Errorf("want %v, got %v", tt.want, got)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }
