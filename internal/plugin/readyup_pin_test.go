package plugin

import "testing"

func TestReadyUpPinFromEnvUsesDefaultWhenOverridesAreUnset(t *testing.T) {
	t.Setenv("READYUP_URL", "")
	t.Setenv("READYUP_SHA256", "")
	t.Setenv("READYUP_DISABLED", "")

	pin, err := ReadyUpPinFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if pin == nil || pin.URL != defaultReadyUpURL || pin.SHA256 != defaultReadyUpSHA256 {
		t.Fatalf("Ready Up pin = %#v, want default URL and SHA-256", pin)
	}
}

func TestReadyUpPinFromEnvRequiresBothOverrides(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
		hash string
	}{
		{name: "URL only", url: "https://example.test/readyup.zip"},
		{name: "SHA-256 only", hash: defaultReadyUpSHA256},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("READYUP_URL", test.url)
			t.Setenv("READYUP_SHA256", test.hash)
			t.Setenv("READYUP_DISABLED", "")
			if pin, err := ReadyUpPinFromEnv(); err == nil || pin != nil {
				t.Fatalf("ReadyUpPinFromEnv() = %#v, %v, want paired override error", pin, err)
			}
		})
	}
}

func TestReadyUpPinFromEnvUsesPairedOverrides(t *testing.T) {
	wantURL := "https://example.test/readyup.zip"
	wantSHA := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("READYUP_URL", wantURL)
	t.Setenv("READYUP_SHA256", wantSHA)
	t.Setenv("READYUP_DISABLED", "")

	pin, err := ReadyUpPinFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if pin == nil || pin.URL != wantURL || pin.SHA256 != wantSHA {
		t.Fatalf("Ready Up pin = %#v, want paired overrides", pin)
	}
}

func TestReadyUpPinFromEnvCanBeDisabled(t *testing.T) {
	t.Setenv("READYUP_URL", "")
	t.Setenv("READYUP_SHA256", "")
	t.Setenv("READYUP_DISABLED", "true")

	pin, err := ReadyUpPinFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if pin != nil {
		t.Fatalf("ReadyUpPinFromEnv() = %#v, want nil when disabled", pin)
	}
}
