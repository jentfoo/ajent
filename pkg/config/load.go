package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/go-analyze/bulk"
)

// ConfigFileName is the user and project preferences file.
const ConfigFileName = "config.json"

// LocalConfigFileName is the gitignored per-workspace override file.
const LocalConfigFileName = "config.local.json"

// Options configures Load. Workspace roots the project layers, Env resolves
// AJENT_* variables (nil uses os.Getenv), and Flags is a caller built layer that
// wins over everything except session overrides.
type Options struct {
	Workspace string
	Env       func(string) string
	Flags     Layer // name "flag", empty Data means no flags were set
}

// Set is the resolved configuration for one workspace. It answers typed settings,
// explains a key's source, and applies session or saved overrides.
type Set struct {
	defaults Layer
	user     Layer
	project  Layer
	local    Layer
	env      Layer
	flags    Layer
	session  Layer // mutable, above every file layer

	workspace string
	userPath  string
	projPath  string
	localPath string
	mu        sync.Mutex // serializes layer writes and resolve, so Resolved stays immutable once built
	resolved  Resolved   // memoized merge
	valid     bool       // false after any layer mutation until the next resolve
}

// Load reads and merges every configuration layer for workspace. Warnings are
// returned rather than fatal so a typo never blocks startup.
func Load(opts Options) (*Set, []string, error) {
	if opts.Workspace == "" {
		opts.Workspace = "."
	}
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}

	var warns []string
	s := &Set{workspace: opts.Workspace, defaults: Defaults()}

	userPath, err := UserPath(ConfigFileName)
	if err != nil {
		return nil, warns, fmt.Errorf("config: %w", err)
	}
	s.userPath = userPath

	data, w, err := loadFile(userPath)
	warns = append(warns, w...)
	if err != nil {
		return nil, warns, err
	}
	if data != nil {
		s.user = Layer{Name: "user", Path: userPath, Data: data}
	}

	projDir := ProjectDir(opts.Workspace)
	s.projPath = filepath.Join(projDir, ConfigFileName)
	data, w, err = loadFile(s.projPath)
	warns = append(warns, w...)
	if err != nil {
		return nil, warns, err
	}
	if data != nil {
		s.project = Layer{Name: "project", Path: s.projPath, Data: data}
	}

	s.localPath = filepath.Join(projDir, LocalConfigFileName)
	data, w, err = loadFile(s.localPath)
	warns = append(warns, w...)
	if err != nil {
		return nil, warns, err
	}
	if data != nil {
		s.local = Layer{Name: "local", Path: s.localPath, Data: data}
	}

	eLayer, eWarns := EnvLayer(env)
	warns = append(warns, eWarns...)
	s.env = eLayer
	s.flags = opts.Flags

	if _, err = Merge(s.layers()...); err != nil {
		return nil, warns, fmt.Errorf("config: %w", err)
	}
	return s, warns, nil
}

// layers returns the configuration stack lowest to highest precedence.
func (s *Set) layers() []Layer {
	return []Layer{s.defaults, s.user, s.project, s.local, s.env, s.flags, s.session}
}

// resolve returns the merged stack, rebuilt only after a layer changed. Safe
// for concurrent use: settings are read while a turn runs and written by key
// handlers on another goroutine.
func (s *Set) resolve() Resolved {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.valid {
		s.resolved, _ = Merge(s.layers()...)
		s.valid = true
	}
	return s.resolved
}

// Settings returns the merged configuration as typed settings.
func (s *Set) Settings() Settings {
	var st Settings
	_ = json.Unmarshal(s.resolve().Bytes(), &st)
	return st
}

// Explain returns the resolved value at key and the layer it came from.
func (s *Set) Explain(key string) (json.RawMessage, string, bool) {
	r := s.resolve()
	return r.Explain(key)
}

// Source returns the layer name that supplied key's value.
func (s *Set) Source(key string) string { return s.resolve().Source(key) }

// SetSession applies key for this session only, above every file layer.
func (s *Set) SetSession(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := SetKey(s.session.Data, key, value)
	if err != nil {
		return err
	}
	s.session = Layer{Name: "session", Data: data}
	s.valid = false
	return nil
}

// ClearSession removes keys from the session layer so lower layers apply again.
// Keys the layer does not declare are ignored; a whole subtree key drops every
// override beneath it.
func (s *Set) ClearSession(keys ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.session.Data
	for _, key := range keys {
		next, err := DeleteKey(data, key)
		if err != nil {
			return err
		}
		data = next
	}
	s.session = Layer{Name: "session", Data: data}
	s.valid = false
	return nil
}

// SessionOverrides returns the dotted leaf paths the session layer declares,
// sorted. Overridden object subtrees flatten to their leaves.
func (s *Set) SessionOverrides() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := parseNode(s.session.Data)
	if err != nil || !root.isObject() {
		return nil
	}
	var out []string
	collectLeaves(root, "", &out)
	slices.Sort(out)
	return out
}

// collectLeaves appends one path per scalar or array under v.
func collectLeaves(v *val, prefix string, out *[]string) {
	for _, k := range v.obj.keys {
		child := v.obj.m[k]
		path := join(prefix, k)
		if child.isObject() && len(child.obj.keys) > 0 {
			collectLeaves(child, path, out)
			continue
		}
		*out = append(*out, path) // a scalar, array or empty object is resettable as-is
	}
}

// SeedSession folds resumed setting overrides into the session layer. Keys
// apply in sorted order so shared object subtrees keep a stable shape across runs.
// A JSON null marks a recorded reset: the key leaves the session layer instead
// of overriding with null, which no lower layer could mean. Returns one warning
// per override that cannot be applied, and the rest still seed.
func (s *Set) SeedSession(overrides map[string]json.RawMessage) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var warns []string
	data := s.session.Data
	keys := bulk.MapKeysSlice(overrides)
	slices.Sort(keys)
	for _, k := range keys {
		v := overrides[k]
		if len(v) == 0 {
			continue
		}
		var next []byte
		var err error
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) { // a recorded reset removes the key
			next, err = DeleteKey(data, k)
		} else {
			next, err = SetKey(data, k, v)
		}
		if err != nil {
			warns = append(warns, fmt.Sprintf("session override %q not applied: %v", k, err))
			continue
		}
		data = next
	}
	s.session = Layer{Name: "session", Data: data}
	s.valid = false
	return warns
}

// Save writes key into the user or project layer file and re-resolves. Warnings
// report content that saving will drop (comments), while errors are write failures.
func (s *Set) Save(layer, key string, value any) ([]string, error) {
	target := s.layerForSave(layer)
	if target == nil {
		return nil, fmt.Errorf("unknown config layer %q", layer)
	}
	path := target.path

	if target.locked {
		release := LockSave()
		defer release()
	}

	var warns []string
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = nil // a brand new file starts empty
	case err != nil:
		return nil, err
	default:
		// a trailing comma is fixed silently; only comments are content lost
		relaxed, hasComments := RelaxJSONReport(raw)
		if hasComments {
			warns = append(warns, path+" has comments; saving will remove them")
		}
		raw = relaxed
	}

	// normalize odd-case keys before SetKey, so a canonical key is never
	// appended beside the twin it would shadow on the next decode
	raw, _ = canonicalizeKeys(raw)

	out, err := SetKey(raw, key, value)
	if err != nil {
		return warns, err
	}
	if err = WriteFileAtomic(path, out, SecretPerm); err != nil {
		return warns, err
	}
	s.mu.Lock()
	target.set(out)
	s.mu.Unlock()
	return warns, nil
}

// layerForSave resolves the mutable named file layer for writing.
func (s *Set) layerForSave(layer string) *fileLayer {
	switch layer {
	case "user":
		return &fileLayer{path: s.userPath, locked: true,
			set: func(d []byte) { s.user = Layer{Name: "user", Path: s.userPath, Data: d}; s.valid = false }}
	case "project":
		// unlocked: a .lock sidecar here is permanent untracked clutter in the user's repo
		return &fileLayer{path: s.projPath,
			set: func(d []byte) { s.project = Layer{Name: "project", Path: s.projPath, Data: d}; s.valid = false }}
	default:
		return nil
	}
}

// fileLayer captures a save target's path and how to refresh the Set.
type fileLayer struct {
	path string
	set  func(data []byte)
	// locked holds the cross-process sidecar lock across the read-modify-write;
	// off for layers whose directory must not grow stray files.
	locked bool
}

// loadFile reads and validates one config file into relaxed data. A missing,
// empty or comment-only file returns (nil, nil), matching how Merge skips blank
// layers. Warnings cover unknown, duplicate and wrong-typed keys.
func loadFile(path string) ([]byte, []string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	data := RelaxJSON(raw)
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil, nil // blank or comment-only content behaves as an absent file
	}

	// odd-case keys normalize here, so merge and provenance see one spelling
	norm, renames := canonicalizeKeys(data)
	data = norm

	var probe map[string]any
	if err = json.Unmarshal(data, &probe); err != nil {
		return nil, nil, JSONError(path, raw, err)
	}

	var warns []string
	for _, r := range renames {
		warns = append(warns, fmt.Sprintf("%s: normalized key %q to %q", path, r.from, r.to))
	}
	// either probe failing means malformed JSON, already reported by the unmarshal check above
	if unknown, err := UnknownKeys(data, Settings{}); err == nil {
		for _, k := range unknown {
			warns = append(warns, fmt.Sprintf("%s: unrecognized key %q", path, k))
		}
	}
	for _, k := range DuplicateKeys(data) {
		warns = append(warns, fmt.Sprintf("%s: duplicate key %q, the last one wins", path, k))
	}
	if te, err := TypeErrors(data, Settings{}); err == nil {
		for _, msg := range te {
			warns = append(warns, fmt.Sprintf("%s: %s", path, msg))
		}
	}
	return data, warns, nil
}
