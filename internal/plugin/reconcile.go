package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/grendalaget/varde-plugin-autotournament/internal/state"
	"github.com/grendalaget/varde-plugin-autotournament/internal/varde"
)

func validateReadyUpPin(pin ReadyUpPin) error {
	parsed, err := url.Parse(pin.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("READYUP_URL must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	if len(pin.SHA256) != 64 || strings.ToLower(pin.SHA256) != pin.SHA256 {
		return errors.New("READYUP_SHA256 must be a lowercase 64-character SHA-256 digest")
	}
	if _, err := hex.DecodeString(pin.SHA256); err != nil {
		return errors.New("READYUP_SHA256 must be a lowercase 64-character SHA-256 digest")
	}
	return nil
}

func validReadyUpLicense(value string) bool {
	return value == "noncommercial" || value == "commercial"
}

func (s *Service) ReconcileAll(ctx context.Context) error {
	if s.readyUp == nil {
		return nil
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	var failures []error
	for groupID, group := range s.state.List() {
		if err := s.reconcileGroup(ctx, groupID, group); err != nil {
			failures = append(failures, fmt.Errorf("group %s: %w", groupID, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Service) ReconcileGroup(ctx context.Context, groupID string) error {
	if s.readyUp == nil {
		return nil
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	group, ok := s.state.Get(groupID)
	if !ok {
		return nil
	}
	return s.reconcileGroup(ctx, groupID, group)
}

func (s *Service) reconcileGroup(ctx context.Context, groupID string, group state.Group) error {
	if !validReadyUpLicense(group.ReadyUpLicense) {
		if !s.warnedLicense[groupID] {
			s.logger.Warn("Ready Up license is not configured or invalid for group; skipping addon reconciliation",
				"group_id", groupID)
			s.warnedLicense[groupID] = true
		}
		return nil
	}
	servers, err := s.varde.ListServers(ctx, groupID)
	if err != nil {
		return fmt.Errorf("list group servers: %w", err)
	}
	var failures []error
	for _, server := range servers {
		if server.GameID != "cs2" {
			continue
		}
		serverID := server.ID
		server, err = s.varde.GetServer(ctx, server.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("get server %s: %w", serverID, err))
			continue
		}
		if server.GameID != "cs2" {
			continue
		}
		acceptedAt, err := s.state.AcceptanceTime(groupID, server.ID, time.Now())
		if err != nil {
			failures = append(failures, fmt.Errorf("get Ready Up acceptance time for server %s: %w", server.ID, err))
			continue
		}
		spec, err := buildReadyUpSpec(group, server, acceptedAt, *s.readyUp)
		if err != nil {
			failures = append(failures, fmt.Errorf("render addon for server %s: %w", server.ID, err))
			continue
		}
		encoded, err := json.Marshal(spec)
		if err != nil {
			failures = append(failures, fmt.Errorf("encode addon for server %s: %w", server.ID, err))
			continue
		}
		digest := sha256.Sum256(encoded)
		hash := hex.EncodeToString(digest[:])
		cacheKey := groupID + "/" + server.ID
		if s.lastSpecHashes[cacheKey] == hash {
			continue
		}
		if err := s.varde.PutAddon(ctx, server.ID, pluginID, "readyup", spec); err != nil {
			var apiErr *varde.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == 400 {
				if !s.warned400[cacheKey] {
					s.logger.Warn("Varde rejected Ready Up addon spec; skipping server",
						"group_id", groupID, "server_id", server.ID, "error", err)
					s.warned400[cacheKey] = true
				}
				continue
			}
			failures = append(failures, fmt.Errorf("put addon for server %s: %w", server.ID, err))
			continue
		}
		s.lastSpecHashes[cacheKey] = hash
		delete(s.warned400, cacheKey)
	}
	return errors.Join(failures...)
}

func buildReadyUpSpec(group state.Group, server varde.Server, acceptedAt string, pin ReadyUpPin) (varde.AddonSpec, error) {
	if !validReadyUpLicense(group.ReadyUpLicense) {
		return varde.AddonSpec{}, errors.New("Ready Up license must be noncommercial or commercial")
	}
	fleetConfig, err := renderFleetConfig(group.ATURL, group.FleetKey, serverPublicAddress(server))
	if err != nil {
		return varde.AddonSpec{}, err
	}
	licenseConfig := fmt.Sprintf(
		"readyup_license_accepted %q\nreadyup_license_accepted_at %q\n",
		group.ReadyUpLicense,
		acceptedAt,
	)
	return varde.AddonSpec{
		URL:     pin.URL,
		SHA256:  pin.SHA256,
		Archive: "zip",
		Target:  "game",
		DriverOptions: map[string]any{
			"gameinfo_search_paths": []string{"csgo/readyup"},
		},
		Files: map[string]string{
			"cfg/ReadyUp/fleet.cfg":   fleetConfig,
			"cfg/ReadyUp/license.cfg": licenseConfig,
		},
	}, nil
}

func renderFleetConfig(atURL, fleetKey, publicAddress string) (string, error) {
	for name, value := range map[string]string{"Auto Tournament URL": atURL, "fleet key": fleetKey, "public address": publicAddress} {
		if strings.ContainsAny(value, "\r\n") {
			return "", fmt.Errorf("%s contains a line break", name)
		}
	}
	parsedURL, err := url.Parse(atURL)
	if err != nil {
		return "", fmt.Errorf("parse Auto Tournament URL: %w", err)
	}
	lines := []string{
		"url = " + atURL,
		"enroll_key = " + fleetKey,
	}
	if publicAddress != "" {
		lines = append(lines, "public_addr = "+publicAddress)
	}
	if strings.EqualFold(parsedURL.Scheme, "http") {
		lines = append(lines, "insecure = 1")
	}
	lines = append(lines, "enabled = 1")
	return strings.Join(lines, "\n") + "\n", nil
}

func serverPublicAddress(server varde.Server) string {
	if server.Service == nil || server.Summary.Address == nil {
		return ""
	}
	address := *server.Summary.Address
	host, addressPort, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	addressPortNumber, portErr := strconv.Atoi(addressPort)
	firstUDPPort := 0
	for _, gamePort := range server.Service.Ports {
		if gamePort.Port > 0 && gamePort.Port <= 65535 && strings.EqualFold(gamePort.Protocol, "udp") {
			if firstUDPPort == 0 {
				firstUDPPort = gamePort.Port
			}
			if portErr == nil && gamePort.Port == addressPortNumber {
				return address
			}
		}
	}
	if firstUDPPort == 0 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(firstUDPPort))
}
