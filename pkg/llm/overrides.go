package llm

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/config"
)

// ApplyOverrides folds config.json's providers and models blocks over f, which
// came from models.json. Warnings name override keys that matched nothing.
func ApplyOverrides(f File, providers, models json.RawMessage) (File, []string, error) {
	var warns []string

	if len(providers) > 0 && string(providers) != "null" && string(providers) != "{}" {
		existing, err := json.Marshal(f.Providers)
		if err != nil {
			return f, warns, err
		}
		merged, err := config.MergeObjects(existing, providers)
		if err != nil {
			return f, warns, fmt.Errorf("llm: provider overrides: %w", err)
		}
		var pm map[string]ProviderConfig
		if err = json.Unmarshal(merged, &pm); err != nil {
			return f, warns, fmt.Errorf("llm: provider overrides: %w", err)
		}
		f.Providers = pm
	}

	if len(models) > 0 && string(models) != "null" && string(models) != "{}" {
		var om map[string]json.RawMessage
		if err := json.Unmarshal(models, &om); err != nil {
			return f, warns, fmt.Errorf("llm: model overrides: %w", err)
		}
		for key, ov := range om {
			pname, id, ok := splitModelKey(key)
			if !ok {
				warns = append(warns, fmt.Sprintf("model override %q has no provider/id shape; ignored", key))
				continue
			}
			pc, found := f.Providers[pname]
			if !found {
				warns = append(warns, fmt.Sprintf("model override %q names unknown provider %q", key, pname))
				continue
			}
			idx := slices.IndexFunc(pc.Models, func(m ModelConfig) bool { return m.ID == id })
			switch {
			case idx >= 0:
				eb, err := json.Marshal(pc.Models[idx])
				if err != nil {
					return f, warns, err
				}
				merged, err := config.MergeObjects(eb, ov)
				if err != nil {
					return f, warns, fmt.Errorf("llm: model override %q: %w", key, err)
				}
				var mc ModelConfig
				if err = json.Unmarshal(merged, &mc); err != nil {
					return f, warns, fmt.Errorf("llm: model override %q: %w", key, err)
				}
				mc.ID = id // the map key is authoritative for identity
				pc.Models[idx] = mc
			default:
				var nc ModelConfig
				if err := json.Unmarshal(ov, &nc); err != nil {
					return f, warns, fmt.Errorf("llm: model override %q: %w", key, err)
				}
				nc.ID = id
				pc.Models = append(pc.Models, nc)
			}
			f.Providers[pname] = pc
		}
	}

	return f, warns, nil
}

// splitModelKey splits a "provider/id" override key on its last slash.
func splitModelKey(key string) (provider, id string, ok bool) {
	i := strings.LastIndexByte(key, '/')
	if i <= 0 || i == len(key)-1 {
		return "", "", false
	}
	return key[:i], key[i+1:], true
}
