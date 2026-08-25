package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Flavor is one runner image variant from the catalog (images/<name>/flavor.json).
type Flavor struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	Base    string `json:"base"`
	Default bool   `json:"default"`
}

// LoadFlavors reads <dir>/<name>/flavor.json for every subdirectory, default
// flavor first, then by name. A missing directory yields an empty catalog (the
// controller then offers "custom" only). Entries that cannot be used are
// skipped and reported in the returned error alongside the usable flavors, so
// one bad directory never stops the controller.
func LoadFlavors(dir string) ([]Flavor, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read flavor catalog %s: %w", dir, err)
	}
	var out []Flavor
	var problems []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), "flavor.json")
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if !flavorNamePattern.MatchString(e.Name()) {
			problems = append(problems, fmt.Errorf("flavor %q: name must be lowercase letters, digits and dashes (max 63 characters)", e.Name()))
			continue
		}
		var f Flavor
		if err := json.Unmarshal(data, &f); err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", path, err))
			continue
		}
		if f.Label == "" || f.Base == "" {
			problems = append(problems, fmt.Errorf("%s: \"label\" and \"base\" are required", path))
			continue
		}
		f.Name = e.Name()
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Name < out[j].Name
	})
	return out, errors.Join(problems...)
}
