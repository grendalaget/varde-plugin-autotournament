package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	fileName = "groups.json"
	keyInfo  = "varde-plugin-autotournament/state"
)

type Group struct {
	ATURL          string
	ATToken        string
	FleetKey       string
	ReadyUpLicense string
	AcceptedAt     map[string]string
}

type sealedValue struct {
	Value string `json:"value"`
}

type diskGroup struct {
	ATURL          string            `json:"at_url"`
	ATToken        sealedValue       `json:"at_token"`
	FleetKey       sealedValue       `json:"fleet_key"`
	ReadyUpLicense string            `json:"readyup_license,omitempty"`
	AcceptedAt     map[string]string `json:"accepted_at,omitempty"`
}

type Store struct {
	mu     sync.RWMutex
	path   string
	key    []byte
	groups map[string]Group
}

func Open(dir string, pluginSecret []byte) (*Store, error) {
	if len(pluginSecret) < 32 {
		return nil, errors.New("plugin secret must contain at least 32 bytes")
	}
	if dir == "" {
		return nil, errors.New("state directory is required")
	}
	key, err := hkdf.Key(sha256.New, pluginSecret, nil, keyInfo, 32)
	if err != nil {
		return nil, fmt.Errorf("derive state encryption key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	store := &Store{
		path:   filepath.Join(dir, fileName),
		key:    key,
		groups: make(map[string]Group),
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read group state: %w", err)
	}
	var disk map[string]diskGroup
	if err := json.Unmarshal(data, &disk); err != nil {
		return fmt.Errorf("decode group state: %w", err)
	}
	for groupID, item := range disk {
		token, err := s.open(groupID, "at_token", item.ATToken)
		if err != nil {
			return fmt.Errorf("decrypt Auto Tournament token for group %q: %w", groupID, err)
		}
		fleetKey, err := s.open(groupID, "fleet_key", item.FleetKey)
		if err != nil {
			return fmt.Errorf("decrypt fleet key for group %q: %w", groupID, err)
		}
		s.groups[groupID] = Group{
			ATURL:          item.ATURL,
			ATToken:        token,
			FleetKey:       fleetKey,
			ReadyUpLicense: item.ReadyUpLicense,
			AcceptedAt:     cloneMap(item.AcceptedAt),
		}
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("secure group state file: %w", err)
	}
	return nil
}

func (s *Store) Get(groupID string) (Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	group, ok := s.groups[groupID]
	return cloneGroup(group), ok
}

func (s *Store) List() map[string]Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	groups := make(map[string]Group, len(s.groups))
	for groupID, group := range s.groups {
		groups[groupID] = cloneGroup(group)
	}
	return groups
}

func (s *Store) Save(groupID string, group Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.groups[groupID]
	group.AcceptedAt = cloneMap(group.AcceptedAt)
	s.groups[groupID] = group
	if err := s.writeLocked(); err != nil {
		if existed {
			s.groups[groupID] = previous
		} else {
			delete(s.groups, groupID)
		}
		return err
	}
	return nil
}

func (s *Store) Delete(groupID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, ok := s.groups[groupID]
	if !ok {
		return nil
	}
	delete(s.groups, groupID)
	if err := s.writeLocked(); err != nil {
		s.groups[groupID] = previous
		return err
	}
	return nil
}

func (s *Store) AcceptanceTime(groupID, serverID string, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	group, ok := s.groups[groupID]
	if !ok {
		return "", errors.New("group state not found")
	}
	if acceptedAt := group.AcceptedAt[serverID]; acceptedAt != "" {
		return acceptedAt, nil
	}
	if group.AcceptedAt == nil {
		group.AcceptedAt = make(map[string]string)
	}
	acceptedAt := now.UTC().Format(time.RFC3339)
	group.AcceptedAt[serverID] = acceptedAt
	s.groups[groupID] = group
	if err := s.writeLocked(); err != nil {
		delete(group.AcceptedAt, serverID)
		s.groups[groupID] = group
		return "", err
	}
	return acceptedAt, nil
}

func (s *Store) writeLocked() error {
	disk := make(map[string]diskGroup, len(s.groups))
	for groupID, group := range s.groups {
		token, err := s.seal(groupID, "at_token", group.ATToken)
		if err != nil {
			return fmt.Errorf("encrypt Auto Tournament token: %w", err)
		}
		fleetKey, err := s.seal(groupID, "fleet_key", group.FleetKey)
		if err != nil {
			return fmt.Errorf("encrypt fleet key: %w", err)
		}
		disk[groupID] = diskGroup{
			ATURL:          group.ATURL,
			ATToken:        token,
			FleetKey:       fleetKey,
			ReadyUpLicense: group.ReadyUpLicense,
			AcceptedAt:     cloneMap(group.AcceptedAt),
		}
	}
	data, err := json.MarshalIndent(disk, "", "  ")
	if err != nil {
		return fmt.Errorf("encode group state: %w", err)
	}
	file, err := os.CreateTemp(filepath.Dir(s.path), "groups.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary group state: %w", err)
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure temporary group state: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("write temporary group state: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync temporary group state: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary group state: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace group state: %w", err)
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("secure group state: %w", err)
	}
	if directory, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (s *Store) seal(groupID, field, plaintext string) (sealedValue, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return sealedValue{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return sealedValue{}, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return sealedValue{}, err
	}
	ciphertext := aead.Seal(nil, nonce, []byte(plaintext), []byte(groupID+"/"+field))
	combined := append(nonce, ciphertext...)
	return sealedValue{Value: base64.RawStdEncoding.EncodeToString(combined)}, nil
}

func (s *Store) open(groupID, field string, value sealedValue) (string, error) {
	combined, err := base64.RawStdEncoding.DecodeString(value.Value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(combined) < aead.NonceSize() {
		return "", errors.New("encrypted value is truncated")
	}
	nonce, ciphertext := combined[:aead.NonceSize()], combined[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, []byte(groupID+"/"+field))
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func cloneGroup(group Group) Group {
	group.AcceptedAt = cloneMap(group.AcceptedAt)
	return group
}

func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
