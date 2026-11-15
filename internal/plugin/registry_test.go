package plugin

import (
	"encoding/json"
	"os"
	"testing"
)

// The plugin store reads registry.json from the default branch. A release
// bumps pluginVersion and the registry together, and the tag must match both.
func TestRegistryMatchesPluginMetadata(t *testing.T) {
	raw, err := os.ReadFile("../../registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Plugins []struct {
			ID, Version, Author, Repository string
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	m := metadata()
	if len(reg.Plugins) != 1 {
		t.Fatalf("registry lists %d plugins, want 1", len(reg.Plugins))
	}
	got := reg.Plugins[0]
	if got.ID != pluginName || got.Version != m.Version || got.Author != m.Author || got.Repository != m.GitHubRepository {
		t.Fatalf("registry entry %+v does not match plugin metadata %s %s %s %s", got, pluginName, m.Version, m.Author, m.GitHubRepository)
	}
}
