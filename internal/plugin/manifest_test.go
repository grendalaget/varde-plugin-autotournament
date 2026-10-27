package plugin

import (
	"encoding/json"
	"os"
	"testing"
)

func TestManifestExampleContainsRequiredReadyUpLicenseSetting(t *testing.T) {
	data, err := os.ReadFile("../../manifest.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Settings []struct {
			Name        string   `json:"name"`
			Type        string   `json:"type"`
			Options     []string `json:"options"`
			Required    bool     `json:"required"`
			Default     any      `json:"default"`
			Label       string   `json:"label"`
			Placeholder string   `json:"placeholder"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse manifest.example.json: %v", err)
	}
	for _, setting := range manifest.Settings {
		if setting.Name != "readyup_license" {
			continue
		}
		if setting.Type != "select" || !setting.Required ||
			len(setting.Options) != 2 || setting.Options[0] != "noncommercial" ||
			setting.Options[1] != "commercial" || setting.Default != nil ||
			setting.Label != "" || setting.Placeholder != "" {
			t.Fatalf("readyup_license setting = %#v", setting)
		}
		return
	}
	t.Fatal("manifest.example.json has no readyup_license setting")
}
