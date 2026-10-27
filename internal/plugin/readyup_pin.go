package plugin

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

const (
	defaultReadyUpURL    = "https://github.com/Auto-Tournament/ready-up/releases/download/v0.1.0-beta.77/ready-up-full-0.1.0-beta.77-linuxsteamrt64.zip"
	defaultReadyUpSHA256 = "b260c55eaff68cf90d09caa95b6311ad0cf1a8919be7c557064b01bdf1ce295e"
)

func ReadyUpPinFromEnv() (*ReadyUpPin, error) {
	url := strings.TrimSpace(os.Getenv("READYUP_URL"))
	sha256 := strings.TrimSpace(os.Getenv("READYUP_SHA256"))
	if (url == "") != (sha256 == "") {
		return nil, errors.New("READYUP_URL and READYUP_SHA256 must be set together")
	}

	disabled := false
	if value := strings.TrimSpace(os.Getenv("READYUP_DISABLED")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return nil, errors.New("READYUP_DISABLED must be a boolean")
		}
		disabled = parsed
	}
	if disabled {
		return nil, nil
	}

	pin := &ReadyUpPin{
		URL:    defaultReadyUpURL,
		SHA256: defaultReadyUpSHA256,
	}
	if url != "" {
		pin.URL = url
		pin.SHA256 = sha256
	}
	if err := validateReadyUpPin(*pin); err != nil {
		return nil, err
	}
	return pin, nil
}
