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
		path, line, col, err, redactSecretLine(text), strings.Repeat(" ", max(col-1, 0)))
}

// secretKeys are JSON keys whose values must never appear in echoed source lines.
var secretKeys = []string{"apiKey", "token", "secret"}

// redactedMask is what a secret value is replaced with.
const redactedMask = `[redacted]`

// redactSecretLine masks the value of every known-secret key on the line, so a
// malformed file's echoed source cannot leak credentials into logs. Minified
// files carry many members per line, so keys are matched anywhere on it.
func redactSecretLine(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '"' {
			b.WriteByte(line[i])
			i++
			continue
		}
		start, end, ok := secretValueSpan(line, i)
		if !ok {
			b.WriteByte(line[i])
			i++
			continue
		}
		b.WriteString(line[i:start])
		b.WriteString(redactedMask)
		i = end
	}
	return b.String()
}

// secretValueSpan reports the span of text to mask for a known-secret key whose
// quoted name starts at line[i]. The name must sit in key position: a quote
// directly following a colon is a value that merely spells a key name, as in
// "model": "token", so it is left alone. A string value is masked exactly;
// anything else, or a missing colon, leaves the value's end unknown and masks
// the rest of the line. ok is false when there is no value to hide.
func secretValueSpan(line string, i int) (start, end int, ok bool) {
	if p := prevNonBlank(line, i); p >= 0 && line[p] == ':' {
		return 0, 0, false // value position: a string spelling a key name
	}
	var key string
	for _, k := range secretKeys {
		if strings.HasPrefix(line[i:], `"`+k+`"`) {
			key = k
			break
		}
	}
	if key == "" {
		return 0, 0, false
	}

	j := skipSpace(line, i+len(key)+2) // just past the closing quote of the name
	if j >= len(line) {
		return 0, 0, false // nothing follows the key on this line
	}
	if line[j] != ':' { // a broken pair hides everything after the name
		return j, len(line), true
	}
	start = skipSpace(line, j+1)
	if start >= len(line) {
		return 0, 0, false // nothing follows the colon on this line
	}
	if line[start] != '"' { // a bare value has no reliable end
		return start, len(line), true
	}
	for j = start + 1; j < len(line); j++ {
		switch line[j] {
		case '\\':
			j++
		case '"':
			return start, j + 1, true
		}
	}
	return start, len(line), true // the string never closes
}

// skipSpace returns the index of the first byte at or after i that is not blank.
func skipSpace(line string, i int) int {
	for ; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' && line[i] != '\r' {
			return i
		}
	}
	return i
}

// prevNonBlank returns the index of the closest byte before i that is not blank,
// or -1 at the line's start.
func prevNonBlank(line string, i int) int {
	for i--; i >= 0; i-- {
		if line[i] != ' ' && line[i] != '\t' && line[i] != '\r' {
			return i
		}
	}
	return -1
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
