package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncryptedStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	secret := []byte(strings.Repeat("s", 32))
	store, err := Open(dir, secret)
	if err != nil {
		t.Fatal(err)
	}
	want := Group{
		ATURL:          "https://at.example",
		ATToken:        "auto-token-value",
		FleetKey:       "fleet-key-value",
		ReadyUpLicense: "noncommercial",
		AcceptedAt:     map[string]string{"server-1": "2025-01-02T03:04:05Z"},
	}
	if err := store.Save("group-1", want); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, fileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), want.ATToken) || strings.Contains(string(data), want.FleetKey) {
		t.Fatal("state file contains an unencrypted credential")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state file mode = %04o, want 0600", got)
	}

	reopened, err := Open(dir, secret)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reopened.Get("group-1")
	if !ok {
		t.Fatal("saved group not found after reopening")
	}
	if got.ATURL != want.ATURL || got.ATToken != want.ATToken || got.FleetKey != want.FleetKey ||
		got.ReadyUpLicense != want.ReadyUpLicense ||
		got.AcceptedAt["server-1"] != want.AcceptedAt["server-1"] {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}
}

func TestEncryptedStateAssociatedDataMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]diskGroup)
	}{
		{
			name: "group",
			mutate: func(groups map[string]diskGroup) {
				groups["other-group"] = groups["group-1"]
				delete(groups, "group-1")
			},
		},
		{
			name: "field",
			mutate: func(groups map[string]diskGroup) {
				group := groups["group-1"]
				group.ATToken, group.FleetKey = group.FleetKey, group.ATToken
				groups["group-1"] = group
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			secret := []byte(strings.Repeat("s", 32))
			store, err := Open(dir, secret)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save("group-1", Group{ATToken: "token", FleetKey: "fleet"}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, fileName)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var groups map[string]diskGroup
			if err := json.Unmarshal(data, &groups); err != nil {
				t.Fatal(err)
			}
			test.mutate(groups)
			data, err = json.Marshal(groups)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir, secret); err == nil {
				t.Fatal("opening state with mismatched associated data succeeded")
			}
		})
	}
}
