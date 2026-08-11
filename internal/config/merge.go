package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Standard layer locations. fctl writes OverridePath into the rootfs before
// each boot; vmpack writes BasePath at image build time.
const (
	BasePath     = "/etc/vminit/config.json"
	OverridePath = "/etc/vminit/override.json"
)

// Merge layers override on top of base and returns the merged document.
//
// The merge is shallow per top-level key: a key present in override
// replaces the base value wholesale, which is what makes `"network": {...}`
// in an override a complete restatement of the network rather than a patch.
// Two exceptions merge per variable instead, because layering individual
// environment variables is the entire point of having layers:
//
//   - extra_env, key by key
//   - image.env, variable by variable, preserving the base order and
//     appending variables the base did not have
//
// image is otherwise merged key by key as well: an override that sets
// image.user must not blank out the entrypoint it said nothing about.
//
// Presence is what counts, not emptiness. `"extra_env": {}` in an override
// is a statement that there are no extra variables; omitting the key says
// nothing. Working on raw JSON rather than decoded structs is what keeps
// that distinction, without every optional field having to become a
// pointer.
func Merge(base, override []byte) ([]byte, error) {
	baseKeys, err := topLevel(base, "base")
	if err != nil {
		return nil, err
	}
	overKeys, err := topLevel(override, "override")
	if err != nil {
		return nil, err
	}

	for k, v := range overKeys {
		switch k {
		case "extra_env":
			merged, err := mergeObjects(baseKeys[k], v)
			if err != nil {
				return nil, fmt.Errorf("extra_env: %w", err)
			}
			baseKeys[k] = merged
		case "image":
			merged, err := mergeImage(baseKeys[k], v)
			if err != nil {
				return nil, fmt.Errorf("image: %w", err)
			}
			baseKeys[k] = merged
		default:
			baseKeys[k] = v
		}
	}

	return json.Marshal(baseKeys)
}

func topLevel(doc []byte, what string) (map[string]json.RawMessage, error) {
	keys := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(doc)) == 0 {
		return keys, nil
	}
	if err := json.Unmarshal(doc, &keys); err != nil {
		return nil, fmt.Errorf("%s config is not a JSON object: %w", what, err)
	}
	return keys, nil
}

// mergeObjects merges two JSON objects key by key, override winning. A null
// or absent base is treated as empty.
func mergeObjects(base, override json.RawMessage) (json.RawMessage, error) {
	merged, err := topLevel(base, "base")
	if err != nil {
		return nil, err
	}
	over, err := topLevel(override, "override")
	if err != nil {
		return nil, err
	}
	for k, v := range over {
		merged[k] = v
	}
	return json.Marshal(merged)
}

func mergeImage(base, override json.RawMessage) (json.RawMessage, error) {
	merged, err := topLevel(base, "base")
	if err != nil {
		return nil, err
	}
	over, err := topLevel(override, "override")
	if err != nil {
		return nil, err
	}
	for k, v := range over {
		if k != "env" {
			merged[k] = v
			continue
		}
		envv, err := mergeEnvLists(merged[k], v)
		if err != nil {
			return nil, fmt.Errorf("env: %w", err)
		}
		merged[k] = envv
	}
	return json.Marshal(merged)
}

// mergeEnvLists merges two OCI-form (KEY=value) environment lists per
// variable. Base order is preserved; a variable the base already declares
// is replaced in place, so PATH does not migrate to the end of the list
// just because an override restated it.
func mergeEnvLists(base, override json.RawMessage) (json.RawMessage, error) {
	var baseList, overList []string
	if len(base) > 0 {
		if err := json.Unmarshal(base, &baseList); err != nil {
			return nil, fmt.Errorf("base is not a list of strings: %w", err)
		}
	}
	if len(override) > 0 {
		if err := json.Unmarshal(override, &overList); err != nil {
			return nil, fmt.Errorf("override is not a list of strings: %w", err)
		}
	}

	out := append([]string(nil), baseList...)
	at := make(map[string]int, len(out))
	for i, e := range out {
		at[envKey(e)] = i
	}
	for _, e := range overList {
		if i, ok := at[envKey(e)]; ok {
			out[i] = e
			continue
		}
		at[envKey(e)] = len(out)
		out = append(out, e)
	}
	return json.Marshal(out)
}

func envKey(entry string) string {
	if i := strings.IndexByte(entry, '='); i >= 0 {
		return entry[:i]
	}
	return entry
}

// Load reads the build-time config, merges the boot-time override if one is
// present, applies defaults and validates the result.
//
// A missing base config is fatal: vminit has nothing to configure the
// machine from. A malformed override is fatal too, and specifically must not
// degrade to the base config, because the override is where the network
// arrives from fctl and a VM booting with the wrong address is worse than
// one that refuses to boot.
func Load(basePath, overridePath string) (*Config, error) {
	base, err := os.ReadFile(basePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", basePath, err)
	}

	merged := base
	override, err := os.ReadFile(overridePath)
	switch {
	case err == nil:
		if merged, err = Merge(base, override); err != nil {
			return nil, fmt.Errorf("merge %s: %w", overridePath, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// No per-launch override. Expected for a plain image boot.
	default:
		return nil, fmt.Errorf("read %s: %w", overridePath, err)
	}

	return Parse(merged)
}

// Parse decodes an already-merged document, applies defaults and validates.
func Parse(doc []byte) (*Config, error) {
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}

	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}
