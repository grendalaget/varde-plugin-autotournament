package plugin

import (
	"encoding/json"
	"os"
	"reflect"
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
			setting.Label != "Ready Up licence" || setting.Placeholder != "" {
			t.Fatalf("readyup_license setting = %#v", setting)
		}
		return
	}
	t.Fatal("manifest.example.json has no readyup_license setting")
}

func TestManifestManagedAndManualModes(t *testing.T) {
	data, err := os.ReadFile("../../manifest.example.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Managed struct {
			Game     string            `json:"game"`
			Mode     string            `json:"mode_setting"`
			Config   map[string]string `json:"config"`
			Bindings map[string]string `json:"bindings"`
		} `json:"managed_server"`
		Settings []struct {
			Name    string   `json:"name"`
			Default any      `json:"default"`
			Options []string `json:"options"`
			Help    string   `json:"help"`
			Hidden  bool     `json:"hidden"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Managed.Game != "autotournament" || manifest.Managed.Mode != "hosting_mode" ||
		!reflect.DeepEqual(manifest.Managed.Config, map[string]string{"licence": "readyup_license"}) ||
		!reflect.DeepEqual(manifest.Managed.Bindings, map[string]string{"at_url": "public_url", "at_token": "secret:api_token"}) {
		t.Fatalf("managed declaration = %#v", manifest.Managed)
	}
	found := false
	for _, setting := range manifest.Settings {
		if setting.Name == "hosting_mode" {
			found = true
			if setting.Default != "Run it on one of our PCs" || !reflect.DeepEqual(setting.Options,
				[]string{"Run it on one of our PCs", "I already have Auto Tournament"}) {
				t.Fatalf("hosting modes = %#v", setting)
			}
		}
		if (setting.Name == "at_url" || setting.Name == "at_token") && setting.Hidden {
			t.Fatal("manual inputs must stay visible in the manifest")
		}
		if setting.Name == "readyup_license" && setting.Help != "This licence choice applies to both Auto Tournament and Ready Up." {
			t.Fatal("licence help must cover both products")
		}
	}
	if !found {
		t.Fatal("hosting mode missing")
	}
}
