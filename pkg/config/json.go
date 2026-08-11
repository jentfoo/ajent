package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RelaxJSON blanks out the two things hand edited configuration files carry that
// encoding/json rejects: // line comments and trailing commas. String literals
// are left untouched.
//
// Blanked rather than removed, so every byte offset still refers to the same
// place in the original file and a later syntax error can be reported against
// what the user actually wrote.
func RelaxJSON(data []byte) []byte {
	out := bytes.Clone(data)
	blankComments(out)
	blankTrailingCommas(out)
	return out
}

// blankComments overwrites // line comments with spaces.
func blankComments(b []byte) {
	var inString, escaped bool
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		}
	}
}

// blankTrailingCommas overwrites a comma followed only by whitespace and a
// closing bracket. Comments are blanked first, so whitespace is all that can
// intervene.
func blankTrailingCommas(b []byte) {
	var inString, escaped bool
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inString:
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == ',':
			j := i + 1
			for j < len(b) && (b[j] == ' ' || b[j] == '\t' || b[j] == '\r' || b[j] == '\n') {
				j++
			}
			if j < len(b) && (b[j] == '}' || b[j] == ']') {
				b[i] = ' '
			}
		}
	}
}

// JSONError returns err with the file, line and column it happened at, plus the
// offending line, so a malformed configuration file says where to look.
func JSONError(path string, data []byte, err error) error {
	offset, ok := jsonErrorOffset(err)
	if !ok {
		return fmt.Errorf("%s: %w", path, err)
	}
	line, col, text := locate(data, offset)
	return fmt.Errorf("%s:%d:%d: %w\n    %s\n    %s^",
		path, line, col, err, text, strings.Repeat(" ", max(col-1, 0)))
}

// jsonErrorOffset returns the byte offset an encoding/json error carries.
func jsonErrorOffset(err error) (int64, bool) {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return syntax.Offset, true
	}
	var typ *json.UnmarshalTypeError
	if errors.As(err, &typ) {
		return typ.Offset, true
	}
	return 0, false
}

// locate converts a byte offset into a one based line and column plus that line.
func locate(data []byte, offset int64) (line, col int, text string) {
	if offset > int64(len(data)) {
		offset = int64(len(data))
	} else if offset < 0 {
		offset = 0
	}
	before := data[:offset]
	line = bytes.Count(before, []byte("\n")) + 1
	start := bytes.LastIndexByte(before, '\n') + 1
	col = int(offset) - start + 1

	rest := data[start:]
	if end := bytes.IndexByte(rest, '\n'); end >= 0 {
		rest = rest[:end]
	}
	return line, col, strings.TrimRight(string(rest), "\r")
}

// DuplicateKeys returns the dotted paths of object keys that appear more than
// once. encoding/json keeps the last silently, so a duplicated key is a setting
// that looks applied and is not.
func DuplicateKeys(data []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(data))
	var dup []string
	if err := walkDuplicates(dec, "", &dup); err != nil {
		return nil // a malformed file is reported by the decode, not here
	}
	return dup
}

// walkDuplicates consumes exactly one JSON value, recording repeated keys.
func walkDuplicates(dec *json.Decoder, path string, dup *[]string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar consumes itself
	}

	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := keyTok.(string)
			child := join(path, key)
			if seen[key] {
				*dup = append(*dup, child)
			}
			seen[key] = true
			if err := walkDuplicates(dec, child, dup); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkDuplicates(dec, path+"[]", dup); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // the closing delimiter
	return err
}
