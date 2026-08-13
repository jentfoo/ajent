package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ConfigFileName is the user and project preferences file.
const ConfigFileName = "config.json"

// LocalConfigFileName is the gitignored per-workspace override file.
const LocalConfigFileName = "config.local.json"

// Options configures Load. Workspace roots the project layers; Env resolves
// AJENT_* variables (nil uses os.Getenv); Flags is a caller built layer that wins
// over everything except session overrides.
type Options struct {
	Workspace string
	Env       func(string) string
	Flags     Layer // name "flag"; empty Data means no flags were set
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
	session  Layer // mutable; above every file layer

	workspace string
	userPath  string
	projPath  string
	localPath string
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
		if hasAPIKey(data) {
			if permWarn := CheckSecretPerms(userPath); permWarn != "" {
				warns = append(warns, permWarn)
			}
		}
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
		stripped, keys := stripKeys(data, "providers.*.apiKey")
		for _, k := range keys {
			warns = append(warns, fmt.Sprintf("%s: ignored apiKey at %q; project files get committed", s.projPath, k))
		}
		s.project = Layer{Name: "project", Path: s.projPath, Data: stripped}
	}

	s.localPath = filepath.Join(projDir, LocalConfigFileName)
	data, w, err = loadFile(s.localPath)
	warns = append(warns, w...)
	if err != nil {
		return nil, warns, err
	}
	if data != nil {
		stripped, keys := stripKeys(data, "providers.*.apiKey")
		for _, k := range keys {
			warns = append(warns, fmt.Sprintf("%s: ignored apiKey at %q; local files are shared", s.localPath, k))
		}
		s.local = Layer{Name: "local", Path: s.localPath, Data: stripped}
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

// resolve merges the current stack.
func (s *Set) resolve() Resolved {
	r, _ := Merge(s.layers()...)
	return r
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
	data, err := SetKey(s.session.Data, key, value)
	if err != nil {
		return err
	}
	s.session = Layer{Name: "session", Data: data}
	return nil
}

// SeedSession folds resumed setting overrides into the session layer.
func (s *Set) SeedSession(overrides map[string]json.RawMessage) {
	data := s.session.Data
	for k, v := range overrides {
		if len(v) == 0 {
			continue
		}
		data, _ = SetKey(data, k, v)
	}
	s.session = Layer{Name: "session", Data: data}
}

// Save writes key into the user or project layer file and re-resolves. Warnings
// report content that saving will drop (comments); errors are write failures.
func (s *Set) Save(layer, key string, value any) ([]string, error) {
	target := s.layerForSave(layer)
	if target == nil {
		return nil, fmt.Errorf("unknown config layer %q", layer)
	}
	path := target.path

	var warns []string
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = nil // a brand new file starts empty
	case err != nil:
		return nil, err
	default:
		if !bytes.Equal(RelaxJSON(raw), raw) {
			warns = append(warns, path+" has comments; saving will remove them")
		}
	}

	out, err := SetKey(raw, key, value)
	if err != nil {
		return warns, err
	}
	if err = WriteFileAtomic(path, out, SecretPerm); err != nil {
		return warns, err
	}
	target.set(out)
	return warns, nil
}

// layerForSave resolves the mutable named file layer for writing.
func (s *Set) layerForSave(layer string) *fileLayer {
	switch layer {
	case "user":
		return &fileLayer{path: s.userPath, set: func(d []byte) { s.user = Layer{Name: "user", Path: s.userPath, Data: d} }}
	case "project":
		return &fileLayer{path: s.projPath, set: func(d []byte) { s.project = Layer{Name: "project", Path: s.projPath, Data: d} }}
	default:
		return nil
	}
}

// fileLayer captures a save target's path and how to refresh the Set.
type fileLayer struct {
	path string
	set  func(data []byte)
}

// loadFile reads and validates one config file into relaxed data. A missing file
// returns (nil, nil); warnings cover unknown and duplicate keys.
func loadFile(path string) ([]byte, []string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	data := RelaxJSON(raw)

	var probe map[string]any
	if err = json.Unmarshal(data, &probe); err != nil {
		return nil, nil, JSONError(path, raw, err)
	}

	var warns []string
	for _, k := range mustUnknownKeys(data) {
		warns = append(warns, fmt.Sprintf("%s: unrecognized key %q", path, k))
	}
	for _, k := range DuplicateKeys(data) {
		warns = append(warns, fmt.Sprintf("%s: duplicate key %q, the last one wins", path, k))
	}
	return data, warns, nil
}

// mustUnknownKeys runs UnknownKeys against Settings, swallowing a decode failure.
func mustUnknownKeys(data []byte) []string {
	unknown, err := UnknownKeys(data, Settings{})
	if err != nil {
		return nil // malformed JSON is reported by the unmarshal check above
	}
	return unknown
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
			if lastSeg(p) == "apiKey" && json.Unmarshal(cv.raw, &s) == nil && s != "" {
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
		if !matchSeg(parts[0], key) {
			continue
		}
		p := append(append([]string{}, path...), key)
		switch {
		case len(parts) == 1:
			if cv.isObject() { // a whole subtree matches the final segment
				var leaves []string
				collectKeys(cv.obj, p, &leaves)
				*removed = append(*removed, leaves...)
			} else {
				*removed = append(*removed, stringsJoin(p))
			}
			removeKey(o, key)
		case cv.isObject():
			stripMatch(cv.obj, parts[1:], p, removed)
		}
	}
}

// collectKeys returns every dotted path of o's leaves for a removal message.
func collectKeys(o *object, prefix []string, out *[]string) {
	for _, k := range o.keys {
		p := append(append([]string{}, prefix...), k)
		if o.m[k].isObject() {
			collectKeys(o.m[k].obj, p, out)
		} else {
			*out = append(*out, stringsJoin(p))
		}
	}
}

func matchSeg(pattern, key string) bool { return pattern == "*" || pattern == key }

// lastSeg returns the final dotted segment of path.
func lastSeg(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// stringsJoin joins a leaf's segments into a dotted path.
func stringsJoin(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	path := ""
	for _, p := range parts[:len(parts)-1] {
		path = join(path, p)
	}
	return join(path, parts[len(parts)-1])
}
