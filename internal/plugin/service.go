package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grendalaget/varde-plugin-autotournament/internal/state"
	"github.com/grendalaget/varde-plugin-autotournament/internal/varde"
)

type ReadyUpPin struct {
	URL    string
	SHA256 string
}

type Config struct {
	Secret         []byte
	AllowPrivateAT bool
	State          *state.Store
	Varde          *varde.Client
	ReadyUp        *ReadyUpPin
	Logger         *slog.Logger
}

type groupChange struct {
	groupID string
	enable  bool
}

type Service struct {
	secret         []byte
	allowPrivateAT bool
	state          *state.Store
	varde          *varde.Client
	readyUp        *ReadyUpPin
	logger         *slog.Logger

	startMu        sync.Mutex
	started        bool
	runWG          sync.WaitGroup
	groupChanges   chan groupChange
	reconcileWake  chan struct{}
	reconcileMu    sync.Mutex
	lastSpecHashes map[string]string
	warned400      map[string]bool
	warnedLicense  map[string]bool
}

func New(cfg Config) (*Service, error) {
	if len(cfg.Secret) < 32 {
		return nil, errors.New("plugin secret must contain at least 32 bytes")
	}
	if cfg.State == nil || cfg.Varde == nil {
		return nil, errors.New("state and Varde clients are required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.ReadyUp != nil {
		if err := validateReadyUpPin(*cfg.ReadyUp); err != nil {
			return nil, err
		}
	}
	return &Service{
		secret:         append([]byte(nil), cfg.Secret...),
		allowPrivateAT: cfg.AllowPrivateAT,
		state:          cfg.State,
		varde:          cfg.Varde,
		readyUp:        cfg.ReadyUp,
		logger:         logger,
		groupChanges:   make(chan groupChange, 256),
		reconcileWake:  make(chan struct{}, 1),
		lastSpecHashes: make(map[string]string),
		warned400:      make(map[string]bool),
		warnedLicense:  make(map[string]bool),
	}, nil
}

func (s *Service) Start(ctx context.Context) error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return errors.New("plugin service already started")
	}
	s.started = true
	if s.readyUp == nil {
		return nil
	}
	s.runWG.Add(1)
	go s.run(ctx)
	return nil
}

func (s *Service) Wait() {
	s.runWG.Wait()
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /varde/v1/settings", s.handleSettings)
	mux.HandleFunc("POST /varde/v1/disable", s.handleDisable)
	mux.HandleFunc("POST /varde/tournaments", s.handleTournaments)
	return mux
}

func (s *Service) run(ctx context.Context) {
	defer s.runWG.Done()
	streams := make(map[string]context.CancelFunc)
	var streamWG sync.WaitGroup
	startStream := func(groupID string) {
		if _, exists := streams[groupID]; exists {
			return
		}
		streamCtx, cancel := context.WithCancel(ctx)
		streams[groupID] = cancel
		streamWG.Add(1)
		go func() {
			defer streamWG.Done()
			s.streamGroupEvents(streamCtx, groupID)
		}()
	}
	stopStream := func(groupID string) {
		if cancel, exists := streams[groupID]; exists {
			cancel()
			delete(streams, groupID)
		}
	}
	defer func() {
		for _, cancel := range streams {
			cancel()
		}
		streamWG.Wait()
	}()

	for groupID := range s.state.List() {
		startStream(groupID)
	}
	if err := s.ReconcileAll(ctx); err != nil {
		s.logger.Error("initial Ready Up reconciliation failed", "error", err)
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-s.groupChanges:
			if change.enable {
				startStream(change.groupID)
			} else {
				stopStream(change.groupID)
			}
		case <-s.reconcileWake:
			if err := s.ReconcileAll(ctx); err != nil {
				s.logger.Error("Ready Up reconciliation failed", "error", err)
			}
		case <-ticker.C:
			if err := s.ReconcileAll(ctx); err != nil {
				s.logger.Error("periodic Ready Up reconciliation failed", "error", err)
			}
		}
	}
}

func (s *Service) streamGroupEvents(ctx context.Context, groupID string) {
	lastEventID := ""
	backoff := time.Second
	for {
		last, err := s.varde.StreamEvents(ctx, groupID, lastEventID, func(event varde.Event) {
			if event.ID > 0 {
				lastEventID = strconv.FormatInt(event.ID, 10)
			}
			if event.Type == "server.created" {
				s.notifyReconcile()
			}
		})
		if last != "" {
			lastEventID = last
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.logger.Warn("Varde event stream disconnected", "group_id", groupID, "error", err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (s *Service) enableGroup(groupID string) {
	s.groupChanges <- groupChange{groupID: groupID, enable: true}
	s.notifyReconcile()
}

func (s *Service) disableGroup(groupID string) {
	s.reconcileMu.Lock()
	for key := range s.lastSpecHashes {
		if strings.HasPrefix(key, groupID+"/") {
			delete(s.lastSpecHashes, key)
		}
	}
	for key := range s.warned400 {
		if strings.HasPrefix(key, groupID+"/") {
			delete(s.warned400, key)
		}
	}
	delete(s.warnedLicense, groupID)
	s.reconcileMu.Unlock()
	s.groupChanges <- groupChange{groupID: groupID}
}

func (s *Service) notifyReconcile() {
	select {
	case s.reconcileWake <- struct{}{}:
	default:
	}
}

func (s *Service) validateGroup(groupID string) error {
	if groupID == "" {
		return errors.New("group_id is required")
	}
	return nil
}

func (s *Service) requireStarted() error {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if !s.started {
		return fmt.Errorf("plugin service is not started")
	}
	return nil
}
