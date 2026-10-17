package tunnelspec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

var ErrSettingsChanged = errors.New("settings changed since the preview; review the latest settings and try again")

// EditPreview contains values safe to show in a terminal or browser. Raw
// configurations and credentials never leave the settings writer.
type EditPreview struct {
	Revision string       `json:"revision"`
	Changes  []ConfigDiff `json:"changes"`
}

type ConfigDiff struct {
	Field  string `json:"field"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func ConfigRevision(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func PreviewChange(before, after []byte) (EditPreview, error) {
	flatten := func(body []byte) (map[string]any, error) {
		var doc map[string]any
		if _, err := toml.Decode(string(body), &doc); err != nil {
			return nil, err
		}
		out := map[string]any{}
		var walk func(string, map[string]any)
		walk = func(prefix string, values map[string]any) {
			for key, value := range values {
				path := prefix + key
				if nested, ok := value.(map[string]any); ok {
					walk(path+".", nested)
				} else {
					out[path] = value
				}
			}
		}
		walk("", doc)
		return out, nil
	}
	old, err := flatten(before)
	if err != nil {
		return EditPreview{}, fmt.Errorf("current config does not parse: %w", err)
	}
	next, err := flatten(after)
	if err != nil {
		return EditPreview{}, fmt.Errorf("new config does not parse: %w", err)
	}
	keys := map[string]bool{}
	for key := range old {
		keys[key] = true
	}
	for key := range next {
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	out := EditPreview{Revision: ConfigRevision(before), Changes: []ConfigDiff{}}
	for _, key := range ordered {
		if reflect.DeepEqual(old[key], next[key]) {
			continue
		}
		out.Changes = append(out.Changes, ConfigDiff{Field: key, Before: previewValue(key, old[key]), After: previewValue(key, next[key])})
	}
	return out, nil
}

func previewValue(key string, value any) string {
	if value == nil {
		return "Not set"
	}
	// Unknown structured values may contain credentials too. Only scalar lists
	// are displayed; tables and nested records are deliberately summarized.
	key = strings.ToLower(key)
	for _, secret := range []string{"token", "password", "secret", "key", "credential", "proxy", "uuid", "auth", "users"} {
		if strings.Contains(key, secret) {
			return "Hidden value"
		}
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "Changed"
	}
	text := string(b)
	if strings.ContainsAny(text, "{@") || strings.Contains(text, "PRIVATE KEY") {
		return "Hidden value"
	}
	if len(text) > 512 {
		return text[:512] + "…"
	}
	return text
}
