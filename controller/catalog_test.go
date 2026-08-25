package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFlavor(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "flavor.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFlavors_DefaultFirstThenByName(t *testing.T) {
	dir := t.TempDir()
	writeFlavor(t, dir, "zeta", `{"label":"Zeta","base":"zeta:1"}`)
	writeFlavor(t, dir, "alpha", `{"label":"Alpha","base":"alpha:1"}`)
	writeFlavor(t, dir, "mid", `{"label":"Mid","base":"mid:1","default":true}`)
	_ = os.MkdirAll(filepath.Join(dir, "test"), 0o755) // no flavor.json: ignored
	flavors, err := LoadFlavors(dir)
	if err != nil {
		t.Fatalf("LoadFlavors: %v", err)
	}
	if len(flavors) != 3 || flavors[0].Name != "mid" || !flavors[0].Default || flavors[1].Name != "alpha" || flavors[2].Name != "zeta" {
		t.Errorf("order: %+v", flavors)
	}
	if flavors[0].Label != "Mid" || flavors[0].Base != "mid:1" {
		t.Errorf("fields: %+v", flavors[0])
	}
}

func TestLoadFlavors_MissingDirIsEmptyAndBadEntriesAreSkippedWithAnError(t *testing.T) {
	if flavors, err := LoadFlavors(filepath.Join(t.TempDir(), "nope")); err != nil || len(flavors) != 0 {
		t.Errorf("missing dir: %v %v", flavors, err)
	}
	dir := t.TempDir()
	writeFlavor(t, dir, "good", `{"label":"Good","base":"good:1","default":true}`)
	writeFlavor(t, dir, "Bad_Name", `{"label":"x","base":"y"}`)
	writeFlavor(t, dir, "nobase", `{"label":"x"}`)
	writeFlavor(t, dir, "broken", `{"label":`)
	writeFlavor(t, dir, strings.Repeat("a", 64), `{"label":"x","base":"y"}`)
	writeFlavor(t, dir, strings.Repeat("b", 63), `{"label":"Long","base":"long:1"}`)
	flavors, err := LoadFlavors(dir)
	if err == nil {
		t.Fatal("unusable entries must be reported")
	}
	for _, want := range []string{"Bad_Name", "nobase", "broken", strings.Repeat("a", 64)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q: %v", want, err)
		}
	}
	if len(flavors) != 2 || flavors[0].Name != "good" || flavors[1].Name != strings.Repeat("b", 63) {
		t.Errorf("usable flavors must still be returned (63-char names are fine): %+v", flavors)
	}
}

func TestLoadFlavors_RealCatalog(t *testing.T) {
	flavors, err := LoadFlavors("../images")
	if err != nil {
		t.Fatalf("LoadFlavors(../images): %v", err)
	}
	if len(flavors) < 3 || flavors[0].Name != "ubuntu" || !flavors[0].Default {
		t.Errorf("real catalog: %+v", flavors)
	}
}
