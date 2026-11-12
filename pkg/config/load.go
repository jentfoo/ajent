package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/go-analyze/bulk"
)

// ConfigFileName is the user and project preferences file.
const ConfigFileName = "config.json"

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
	env      Layer
	flags    Layer
	session  Layer // mutable, above every file layer

	workspace string
	envFn     func(string) string // captured so a reload reads the same environment
	userPath  string
	projPath  string
	mu        sync.Mutex // serializes layer writes and resolve, so Resolved stays immutable once built
	resolved  Resolved   // memoized merge
	valid     bool       // false after any layer mutation until the next resolve
}

// Load reads and merges every configuration layer for workspace in precedence
// order: defaults, user file, project file, AJENT_* environment variables,
// then opts.Flags. Warnings are returned rather than fatal so a typo
// never blocks startup.
func Load(opts Options) (*Set, []string, error) {
	if opts.Workspace == "" {
		opts.Workspace = "."
	}
	env := opts.Env
	if env == nil {
		env = os.Getenv
	}

	userPath, err := UserPath(ConfigFileName)
	if err != nil {
		return nil, nil, fmt.Errorf("config: %w", err)
	}
	projDir := ProjectDir(opts.Workspace)
	s := &Set{
		defaults:  Defaults(),
		flags:     opts.Flags,
		workspace: opts.Workspace,
		envFn:     env,
		userPath:  userPath,
		projPath:  filepath.Join(projDir, ConfigFileName),
	}

	warns, err := s.readLayers()
	if err != nil {
		return nil, warns, err
	}

	resolved, err := Merge(s.layers()...)
	if err != nil {
		return nil, warns, fmt.Errorf("config: %w", err)
	}
	s.mu.Lock()
	s.resolved = resolved
	s.valid = true
	s.mu.Unlock()
	return s, warns, nil
}

// Reload re-reads the user, project and environment layers from disk,
// keeping session overrides and flags. A failure leaves the previous layers in
// place; warnings report what the files themselves are missing or dropping.
func (s *Set) Reload() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLayers()
}

// readLayers reads the file and environment layers, assigning them only once
// every read succeeded. Callers hold the write lock.
func (s *Set) readLayers() ([]string, error) {
	data, warns, err := loadFile(s.userPath)
	if err != nil {
		return warns, err
	}
	var user Layer
	if data != nil {
		if hasAPIKey(data) {
			if permWarn := CheckSecretPerms(s.userPath); permWarn != "" {
				warns = append(warns, permWarn)
			}
		}
		user = Layer{Name: "user", Path: s.userPath, Data: data}
	}

	project, w, err := readSharedLayer("project", s.projPath, "project files get committed")
	warns = append(warns, w...)
	if err != nil {
		return warns, err
	}

	eLayer, eWarns := EnvLayer(s.envFn)
	warns = append(warns, eWarns...)

	s.user, s.project, s.env = user, project, eLayer
	s.valid = false
	return warns, nil
}

// readSharedLayer loads a committed or shared layer file, stripping any literal
// apiKey and warning with reason. A missing file yields a zero Layer.
func readSharedLayer(name, path, reason string) (Layer, []string, error) {
	data, warns, err := loadFile(path)
	if err != nil || data == nil {
		return Layer{}, warns, err
	}
	stripped, keys := stripKeys(data, "providers.*.apiKey")
	for _, k := range keys {
		warns = append(warns, fmt.Sprintf("%s: ignored apiKey at %q; %s", path, k, reason))
	}
	return Layer{Name: name, Path: path, Data: stripped}, warns, nil
}

// layers returns the configuration stack lowest to highest precedence. Callers
// hold the lock.
func (s *Set) layers() []Layer {
	return []Layer{s.defaults, s.user, s.project, s.env, s.flags, s.session}
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
// Keys the layer does not declare are ignored. A whole subtree key drops every
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

// Save writes key into the named file layer ("user" or "project") and
// re-resolves. Warnings report content a landed save dropped, such as //
// comments; an error means nothing was written, so no warnings ride one.
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

	raw, droppedComments, err := ReadForSave(path)
	if err != nil {
		return nil, err
	}

	// normalize odd-case keys before SetKey, so a canonical key is never
	// appended beside the twin it would shadow on the next decode
	raw, _ = canonicalizeKeys(raw)

	out, err := SetKey(raw, key, value)
	if err != nil {
		return nil, JSONError(path, raw, err)
	}
	if err = WriteFileAtomic(path, out, SecretPerm); err != nil {
		return nil, err
	}

	var warns []string
	if droppedComments {
		warns = append(warns, path+" has comments; saving removed them")
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
	data, err := RelaxedObject(path, raw)
	if err != nil {
		return nil, nil, err
	} else if data == nil {
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

// hasAPIKey reports whether any non-empty literal apiKey exists in data.
func hasAPIKey(data []byte) bool {
	v, err := parseNode(data)
	if err != nil || !v.isObject() {
		return false
	}
	found := false
	var scan func(o *object, path string)
	scan = func(o *object, path string) {
		for _, k := range o.keys {
			cv := o.m[k]
			p := join(path, k)
			if cv.isObject() {
				scan(cv.obj, p)
				continue
			}
			var s string
			if k == "apiKey" && json.Unmarshal(cv.raw, &s) == nil && s != "" {
				found = true
			}
		}
	}
	scan(v.obj, "")
	return found
}

// stripKeys removes every value matching pattern and returns the stripped data
// plus the removed dotted paths. A "*" segment matches any single key.
func stripKeys(data []byte, patterns ...string) ([]byte, []string) {
	v, err := parseNode(data)
	if err != nil || !v.isObject() {
		return data, nil
	}
	var removed []string
	for _, pat := range patterns {
		stripMatch(v.obj, splitPath(pat), nil, &removed)
	}
	out, _ := indentJSON(v.marshal())
	return out, removed
}

// stripMatch removes matching leaves from o; path tracks the current dotted prefix.
func stripMatch(o *object, parts []string, path []string, removed *[]string) {
	if len(parts) == 0 {
		return
	}
	for _, key := range o.keys {
		cv := o.m[key]
		if parts[0] != "*" && parts[0] != key {
			continue
		}
		p := append(append([]string{}, path...), key)
		switch {
		case len(parts) == 1:
			if cv.isObject() { // a whole subtree matches the final segment
				collectLeaves(cv, strings.Join(p, "."), removed)
			} else {
				*removed = append(*removed, strings.Join(p, "."))
			}
			removeKey(o, key)
		case cv.isObject():
			stripMatch(cv.obj, parts[1:], p, removed)
		}
	}
}
