package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed web/dist/*
var adminAssets embed.FS

var managerStartedAt = time.Now()

var authLimiter = newAuthLimiter()
var adminSessions = newSessionStore()
var adminConfigPath string

const defaultGeneratedJitsiBase = "https://meet.handyweb.org"

type Config struct {
	Version          int        `json:"version"`
	LegacyVersion    int        `json:"vesion"`
	Name             string     `json:"name"`
	Port             int        `json:"port"`
	SubscriptionPath string     `json:"subscription_path"`
	Refresh          string     `json:"refresh,omitempty"`
	ActiveLocationID string     `json:"active_location_id"`
	Clients          []Client   `json:"clients"`
	Locations        []Location `json:"-"`
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type config Config
	var parsed config
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*c = Config(parsed)
	c.Normalize()
	return nil
}

func (c Config) MarshalJSON() ([]byte, error) {
	type configJSON struct {
		Version          int      `json:"version"`
		Name             string   `json:"name"`
		Port             int      `json:"port"`
		SubscriptionPath string   `json:"subscription_path,omitempty"`
		Refresh          string   `json:"refresh,omitempty"`
		ActiveLocationID string   `json:"active_location_id,omitempty"`
		Clients          []Client `json:"clients"`
	}
	return json.Marshal(configJSON{
		Version:          c.Version,
		Name:             c.Name,
		Port:             c.Port,
		SubscriptionPath: c.SubscriptionPath,
		Refresh:          c.Refresh,
		ActiveLocationID: c.ActiveLocationID,
		Clients:          c.Clients,
	})
}

type Client struct {
	ClientID  string     `json:"client-id"`
	Refresh   string     `json:"refresh,omitempty"`
	Quota     Quota      `json:"quota,omitempty"`
	Locations []Location `json:"locations"`
}

type Quota struct {
	SpeedMbps int    `json:"speed_mbps,omitempty"`
	TrafficGB int    `json:"traffic_gb,omitempty"`
	UsedGB    int    `json:"used_gb,omitempty"`
	UsedBytes uint64 `json:"used_bytes,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type Location struct {
	Name      string      `json:"name"`
	ClientID  string      `json:"client-id"`
	Endpoint  Endpoint    `json:"endpoint"`
	Carrier   string      `json:"carrier"`
	Transport Transport   `json:"transport"`
	Link      string      `json:"link"`
	Data      string      `json:"data"`
	DNS       string      `json:"dns"`
	Proxy     Socks5Proxy `json:"proxy,omitempty"`
}

type Endpoint struct {
	RoomID string `json:"room_id"`
	Key    string `json:"key"`
}

type Socks5Proxy struct {
	Addr string `json:"addr,omitempty"`
	Port int    `json:"port,omitempty"`
	User string `json:"user,omitempty"`
	Pass string `json:"pass,omitempty"`
}

type Transport struct {
	Type    string
	Payload map[string]string
}

type olcrtcRuntimeConfig struct {
	Mode   string             `yaml:"mode"`
	Auth   olcrtcAuthConfig   `yaml:"auth"`
	Room   olcrtcRoomConfig   `yaml:"room,omitempty"`
	Crypto olcrtcCryptoConfig `yaml:"crypto,omitempty"`
	Net    olcrtcNetConfig    `yaml:"net"`
	SOCKS  olcrtcSocksConfig  `yaml:"socks,omitempty"`
	VP8    *olcrtcVP8Config   `yaml:"vp8,omitempty"`
	SEI    *olcrtcSEIConfig   `yaml:"sei,omitempty"`
	Video  *olcrtcVideoConfig `yaml:"video,omitempty"`
	Gen    *olcrtcGenConfig   `yaml:"gen,omitempty"`
	Data   string             `yaml:"data,omitempty"`
	Debug  bool               `yaml:"debug,omitempty"`
	FFmpeg string             `yaml:"ffmpeg,omitempty"`
}

type olcrtcAuthConfig struct {
	Provider string `yaml:"provider"`
}

type olcrtcRoomConfig struct {
	ID string `yaml:"id,omitempty"`
}

type olcrtcCryptoConfig struct {
	Key string `yaml:"key,omitempty"`
}

type olcrtcNetConfig struct {
	Transport string `yaml:"transport,omitempty"`
	DNS       string `yaml:"dns,omitempty"`
}

type olcrtcSocksConfig struct {
	ProxyAddr string `yaml:"proxy_addr,omitempty"`
	ProxyPort int    `yaml:"proxy_port,omitempty"`
	ProxyUser string `yaml:"proxy_user,omitempty"`
	ProxyPass string `yaml:"proxy_pass,omitempty"`
}

type olcrtcVP8Config struct {
	FPS       int `yaml:"fps,omitempty"`
	BatchSize int `yaml:"batch_size,omitempty"`
}

type olcrtcSEIConfig struct {
	FPS          int `yaml:"fps,omitempty"`
	BatchSize    int `yaml:"batch_size,omitempty"`
	FragmentSize int `yaml:"fragment_size,omitempty"`
	AckTimeoutMS int `yaml:"ack_timeout_ms,omitempty"`
}

type olcrtcVideoConfig struct {
	Width      int    `yaml:"width,omitempty"`
	Height     int    `yaml:"height,omitempty"`
	FPS        int    `yaml:"fps,omitempty"`
	Bitrate    string `yaml:"bitrate,omitempty"`
	HW         string `yaml:"hw,omitempty"`
	QRSize     int    `yaml:"qr_size,omitempty"`
	QRRecovery string `yaml:"qr_recovery,omitempty"`
	Codec      string `yaml:"codec,omitempty"`
	TileModule int    `yaml:"tile_module,omitempty"`
	TileRS     int    `yaml:"tile_rs,omitempty"`
}

type olcrtcGenConfig struct {
	Amount int `yaml:"amount"`
}

func (t *Transport) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var typ string
	if err := json.Unmarshal(raw["type"], &typ); err != nil {
		return fmt.Errorf("transport.type: %w", err)
	}

	payload := make(map[string]string)
	for key, value := range raw {
		if key == "type" {
			continue
		}

		if key == "payload" {
			var nested map[string]any
			if err := json.Unmarshal(value, &nested); err != nil {
				return fmt.Errorf("transport.payload: %w", err)
			}
			for payloadKey, payloadValue := range nested {
				payload[payloadKey] = fmt.Sprint(payloadValue)
			}
			continue
		}

		var scalar any
		if err := json.Unmarshal(value, &scalar); err != nil {
			return fmt.Errorf("transport.%s: %w", key, err)
		}
		payload[key] = fmt.Sprint(scalar)
	}

	t.Type = typ
	t.Payload = payload
	return nil
}

func (t Transport) MarshalJSON() ([]byte, error) {
	raw := map[string]any{"type": t.Type}
	if len(t.Payload) != 0 {
		raw["payload"] = t.Payload
	}
	return json.Marshal(raw)
}

type process struct {
	location Location
	cmd      *exec.Cmd
	netns    *netnsRuntime
	logs     *logBuffer
	done     chan error
	started  time.Time
	exited   time.Time
	exitErr  string
	running  bool
	restarts int
	mu       sync.RWMutex
}

type starter func(context.Context, string, Location) (*process, error)

type Supervisor struct {
	mu         sync.RWMutex
	cfg        Config
	olcrtcPath string
	processes  map[string]*process
	start      starter
	quota      *QuotaEnforcer
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var configPath string
	var port int
	var listenAddr string
	flag.StringVar(&configPath, "config", "", "path to olcrtc-manager JSON config")
	flag.IntVar(&port, "port", 0, "HTTP listen port; overrides config.port")
	flag.StringVar(&listenAddr, "addr", envDefault("OLCRTC_MANAGER_ADDR", "127.0.0.1"), "HTTP listen address")
	flag.Parse()

	if configPath == "" {
		return errors.New("-config is required")
	}
	adminConfigPath = configPath

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	if port != 0 {
		cfg.Port = port
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	olcrtcPath, err := resolveOlcrtcPath()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	supervisor := NewSupervisor(olcrtcPath, startInstance)
	quotaEnforcer := NewQuotaEnforcer(configPath, supervisor)
	supervisor.SetQuotaEnforcer(quotaEnforcer)
	if err := supervisor.StartAll(ctx, cfg); err != nil {
		return err
	}
	defer supervisor.StopAll()
	go quotaEnforcer.Run(ctx)

	reloadc := make(chan os.Signal, 1)
	signal.Notify(reloadc, syscall.SIGHUP)
	defer signal.Stop(reloadc)

	reload := func() error {
		reloaded, err := loadConfig(configPath)
		if err != nil {
			return err
		}
		if port != 0 {
			reloaded.Port = port
		}
		if reloaded.Port != cfg.Port {
			return fmt.Errorf("reload cannot change port from %d to %d", cfg.Port, reloaded.Port)
		}
		if err := reloaded.Validate(); err != nil {
			return err
		}
		return supervisor.Reload(ctx, reloaded)
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/-/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isLoopbackRequest(r) {
			http.Error(w, "reload is only allowed from loopback", http.StatusForbidden)
			return
		}
		if err := reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	adminFileServer, err := adminFileServer()
	if err != nil {
		return err
	}
	handler.Handle("/admin", http.HandlerFunc(adminPageHandler(adminFileServer)))
	handler.Handle("/assets/", adminFileServer)
	handler.Handle("/api/auth/login", http.HandlerFunc(loginHandler(configPath)))
	handler.Handle("/api/auth/setup", http.HandlerFunc(setupHandler(configPath)))
	handler.Handle("/api/auth/logout", adminAuth(http.HandlerFunc(logoutHandler)))
	handler.Handle("/api/auth/me", http.HandlerFunc(authMeHandler(configPath)))
	handler.Handle("/api/auth/password", adminAuth(http.HandlerFunc(changePasswordHandler(configPath))))
	handler.Handle("/api/settings", adminAuth(http.HandlerFunc(settingsHandler(configPath, supervisor, port != 0))))
	handler.Handle("/api/reload", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	handler.Handle("/api/state", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, supervisor.State())
	})))
	handler.Handle("/api/metrics", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, collectMetrics(supervisor))
	})))
	handler.Handle("/api/audit", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"events": readAudit(configPath, 100)})
	})))
	handler.Handle("/api/logs/", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		clientID, roomID, transport := logRequestTarget(r)
		if clientID == "" || roomID == "" || transport == "" {
			http.NotFound(w, r)
			return
		}
		lines, ok := supervisor.Logs(clientID, roomID, transport)
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string][]LogLine{"logs": lines})
	})))
	handler.Handle("/api/actions/restart", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req locationActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := supervisor.Restart(r.Context(), req.ClientID, req.RoomID, req.Transport); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	handler.Handle("/api/actions/regenerate-room", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req clientActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := regenerateClientRoom(r.Context(), configPath, olcrtcPath, req.ClientID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	handler.Handle("/api/actions/rotate-key", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req clientActionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := rotateClientKey(configPath, req.ClientID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	handler.Handle("/api/tools/generate-room", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req generateRoomRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.Carrier = strings.TrimSpace(req.Carrier)
		req.DNS = strings.TrimSpace(req.DNS)
		if req.Carrier == "" {
			http.Error(w, "carrier is required", http.StatusBadRequest)
			return
		}
		if req.DNS == "" {
			req.DNS = "1.1.1.1:53"
		}
		roomID, err := generateRoomID(r.Context(), olcrtcPath, req.Carrier, req.DNS)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]string{"room_id": roomID})
	})))
	handler.Handle("/api/clients", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		clientID, err := addClientFromRequest(r.Context(), configPath, olcrtcPath, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]string{"client_id": clientID})
	})))
	handler.Handle("/api/clients/", adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete && r.Method != http.MethodPut && r.Method != http.MethodPost {
			w.Header().Set("Allow", "DELETE, PUT, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/clients/")
		if strings.HasSuffix(rest, "/locations") && r.Method == http.MethodPost {
			clientID := strings.TrimSuffix(rest, "/locations")
			if err := addLocationFromRequest(r.Context(), configPath, olcrtcPath, clientID, r); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		if strings.Contains(rest, "/locations/") && r.Method == http.MethodDelete {
			parts := strings.Split(rest, "/locations/")
			if len(parts) != 2 {
				http.NotFound(w, r)
				return
			}
			if err := deleteLocation(configPath, parts[0], parts[1]); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		clientID := rest
		if clientID == "" || strings.Contains(clientID, "/") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodDelete:
			if err := deleteClient(configPath, clientID); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPut:
			if err := updateClientFromRequest(r.Context(), configPath, olcrtcPath, clientID, r); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := reload(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})))
	handler.Handle("/", subscriptionHandler(supervisor))

	server := &http.Server{
		Addr:              net.JoinHostPort(listenAddr, strconv.Itoa(cfg.Port)),
		Handler:           securityHeaders(handler),
		ReadHeaderTimeout: 5 * time.Second,
	}
	tlsCert, tlsKey, tlsEnabled, err := tlsFilesFromEnv()
	if err != nil {
		return err
	}

	errc := make(chan error, 1)
	go func() {
		log.Printf("serving subscription and admin panel on %s", server.Addr)
		var err error
		if tlsEnabled {
			log.Printf("TLS enabled with certificate %s", tlsCert)
			err = server.ListenAndServeTLS(tlsCert, tlsKey)
		} else {
			err = server.ListenAndServe()
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return server.Shutdown(shutdownCtx)
		case <-reloadc:
			if err := reload(); err != nil {
				log.Printf("reload failed: %v", err)
				continue
			}
			log.Printf("reload completed")
		case err := <-errc:
			return err
		}
	}
}

func NewSupervisor(olcrtcPath string, start starter) *Supervisor {
	return &Supervisor{
		olcrtcPath: olcrtcPath,
		processes:  make(map[string]*process),
		start:      start,
	}
}

func (s *Supervisor) SetQuotaEnforcer(quota *QuotaEnforcer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quota = quota
}

func (s *Supervisor) StartAll(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, loc := range activeLocations(cfg, time.Now()) {
		p, err := s.start(ctx, s.olcrtcPath, loc)
		if err != nil {
			stopProcessMap(s.processes)
			s.processes = make(map[string]*process)
			return err
		}
		s.registerQuotaLocked(loc, quotaForClient(cfg, loc.ClientID), p)
		key := locationKey(loc)
		s.processes[key] = p
		s.monitorProcess(ctx, key, p)
	}
	s.cfg = cfg
	return nil
}

func (s *Supervisor) Reload(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	next := locationsByKey(activeLocations(cfg, time.Now()))

	s.mu.Lock()
	defer s.mu.Unlock()

	current := s.runningLocationsLocked()
	started := make(map[string]*process)

	for id, nextLoc := range next {
		currentLoc, exists := current[id]
		if exists && reflect.DeepEqual(currentLoc, nextLoc) {
			if p := s.processes[id]; p != nil {
				s.registerQuotaLocked(nextLoc, quotaForClient(cfg, nextLoc.ClientID), p)
			}
			continue
		}

		p, err := s.start(ctx, s.olcrtcPath, nextLoc)
		if err != nil {
			stopProcessMap(started)
			return err
		}
		s.registerQuotaLocked(nextLoc, quotaForClient(cfg, nextLoc.ClientID), p)
		started[id] = p
	}

	for id, currentLoc := range current {
		nextLoc, exists := next[id]
		if !exists || !reflect.DeepEqual(currentLoc, nextLoc) {
			s.stopLocked(id)
		}
	}

	for id, p := range started {
		s.processes[id] = p
		s.monitorProcess(ctx, id, p)
	}
	s.cfg = cfg
	return nil
}

func (s *Supervisor) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quota != nil {
		for id := range s.processes {
			s.quota.Unregister(id)
		}
	}
	stopProcessMap(s.processes)
	s.processes = make(map[string]*process)
}

func (s *Supervisor) UpdateSettings(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Name = cfg.Name
	s.cfg.Port = cfg.Port
	s.cfg.SubscriptionPath = cfg.SubscriptionPath
	s.cfg.Refresh = cfg.Refresh
}

func (s *Supervisor) Subscription(now time.Time) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return subscription(s.cfg, now)
}

func (s *Supervisor) SubscriptionForClient(clientID string, now time.Time) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return subscriptionForClient(s.cfg, clientID, now)
}

func (s *Supervisor) SubscriptionPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.SubscriptionPath
}

func (s *Supervisor) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	clients := make(map[string][]LocationState)
	peerCount := 0
	for _, loc := range s.cfg.Locations {
		key := locationKey(loc)
		p, exists := s.processes[key]
		runtime := RuntimeState{Status: "stopped"}
		if exists {
			runtime = p.state()
		}
		if runtime.PeerCount != nil {
			peerCount += *runtime.PeerCount
		}
		clients[loc.ClientID] = append(clients[loc.ClientID], LocationState{
			Name:      loc.Name,
			RoomID:    loc.Endpoint.RoomID,
			Key:       loc.Endpoint.Key,
			URI:       locationURI(loc),
			Carrier:   loc.Carrier,
			Transport: loc.Transport.Type,
			Payload:   loc.Transport.Payload,
			Link:      loc.Link,
			DNS:       loc.DNS,
			Proxy:     loc.Proxy,
			Running:   runtime.Running,
			Runtime:   runtime,
		})
	}

	clientIDs := make([]string, 0, len(clients))
	for id := range clients {
		clientIDs = append(clientIDs, id)
		sort.Slice(clients[id], func(i, j int) bool {
			return clients[id][i].Name < clients[id][j].Name
		})
	}
	sort.Strings(clientIDs)

	out := State{
		Name:             s.cfg.Name,
		Port:             s.cfg.Port,
		SubscriptionPath: s.cfg.SubscriptionPath,
		Refresh:          s.cfg.Refresh,
		ClientCount:      len(clientIDs),
		RunningCount:     s.runningCountLocked(),
		PeerCount:        peerCount,
		Clients:          make([]ClientState, 0, len(clientIDs)),
	}
	for _, id := range clientIDs {
		quota := Quota{}
		refresh := ""
		for _, client := range s.cfg.Clients {
			if client.ClientID == id {
				quota = client.Quota
				refresh = client.Refresh
				break
			}
		}
		out.Clients = append(out.Clients, ClientState{
			ClientID:  id,
			Refresh:   refresh,
			Quota:     quota,
			Locations: clients[id],
		})
	}
	return out
}

func (s *Supervisor) Logs(clientID, roomID, transport string) ([]LogLine, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	p, ok := s.processes[strings.Join([]string{clientID, roomID, transport}, ":")]
	if !ok || p.logs == nil {
		return nil, false
	}
	return p.logs.Snapshot(), true
}

func (s *Supervisor) Restart(ctx context.Context, clientID, roomID, transport string) error {
	key := strings.Join([]string{strings.TrimSpace(clientID), strings.TrimSpace(roomID), strings.TrimSpace(transport)}, ":")

	s.mu.Lock()
	p, ok := s.processes[key]
	if !ok {
		loc, found := s.locationLocked(key)
		if !found {
			s.mu.Unlock()
			return fmt.Errorf("location %q not found", key)
		}
		quota := s.clientQuotaLocked(loc.ClientID)
		if quotaStatus(quota, time.Now()) != "active" {
			s.mu.Unlock()
			return fmt.Errorf("location %q is blocked by quota status %s", key, quotaStatus(quota, time.Now()))
		}
		next, err := s.start(context.Background(), s.olcrtcPath, loc)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.registerQuotaLocked(loc, quota, next)
		s.processes[key] = next
		s.monitorProcess(ctx, key, next)
		s.mu.Unlock()
		return nil
	}
	loc := p.location
	s.stopLocked(key)
	s.mu.Unlock()

	if err := waitProcessStopped(ctx, p, 5*time.Second); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := s.start(context.Background(), s.olcrtcPath, loc)
	if err != nil {
		return err
	}
	s.registerQuotaLocked(loc, s.clientQuotaLocked(loc.ClientID), next)
	s.processes[key] = next
	s.monitorProcess(ctx, key, next)
	return nil
}

func (s *Supervisor) monitorProcess(ctx context.Context, key string, p *process) {
	go func() {
		err := <-p.done
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("olcrtc for %s exited: %v", key, err)
		}
		time.Sleep(time.Duration(min(p.restarts+1, 5)) * time.Second)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.processes[key] != p || ctx.Err() != nil {
			return
		}
		if p.restarts >= 3 {
			log.Printf("olcrtc for %s reached restart limit", key)
			return
		}
		next, startErr := s.start(ctx, s.olcrtcPath, p.location)
		if startErr != nil {
			log.Printf("olcrtc restart for %s failed: %v", key, startErr)
			return
		}
		s.registerQuotaLocked(p.location, s.clientQuotaLocked(p.location.ClientID), next)
		next.restarts = p.restarts + 1
		s.processes[key] = next
		s.monitorProcess(ctx, key, next)
	}()
}

func (s *Supervisor) registerQuotaLocked(loc Location, quota Quota, p *process) {
	if s.quota == nil {
		return
	}
	if err := s.quota.Register(loc, quota, p); err != nil {
		log.Printf("quota accounting unavailable for %s: %v", locationKey(loc), err)
	}
}

func (s *Supervisor) clientQuotaLocked(clientID string) Quota {
	for _, client := range s.cfg.Clients {
		if client.ClientID == clientID {
			return client.Quota
		}
	}
	return Quota{}
}

func (s *Supervisor) runningLocationsLocked() map[string]Location {
	current := make(map[string]Location, len(s.processes))
	for id, p := range s.processes {
		if p != nil {
			current[id] = p.location
		}
	}
	return current
}

func (s *Supervisor) locationLocked(key string) (Location, bool) {
	for _, loc := range s.cfg.Locations {
		if locationKey(loc) == key {
			return loc, true
		}
	}
	return Location{}, false
}

func quotaForClient(cfg Config, clientID string) Quota {
	for _, client := range cfg.Clients {
		if client.ClientID == clientID {
			return client.Quota
		}
	}
	return Quota{}
}

func (s *Supervisor) ApplyQuotaConfig(cfg Config, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	for _, client := range cfg.Clients {
		if quotaStatus(client.Quota, now) == "active" {
			for _, loc := range client.Locations {
				if p := s.processes[locationKey(loc)]; p != nil {
					s.registerQuotaLocked(loc, client.Quota, p)
				}
			}
			continue
		}
		for _, loc := range client.Locations {
			s.stopLocked(locationKey(loc))
		}
	}
}

func (s *Supervisor) runningCountLocked() int {
	count := 0
	for _, p := range s.processes {
		if p.state().Running {
			count++
		}
	}
	return count
}

type State struct {
	Name             string        `json:"name"`
	Port             int           `json:"port"`
	SubscriptionPath string        `json:"subscription_path"`
	Refresh          string        `json:"refresh,omitempty"`
	ClientCount      int           `json:"client_count"`
	RunningCount     int           `json:"running_count"`
	PeerCount        int           `json:"peer_count"`
	Clients          []ClientState `json:"clients"`
}

type ClientState struct {
	ClientID  string          `json:"client_id"`
	Refresh   string          `json:"refresh,omitempty"`
	Quota     Quota           `json:"quota"`
	Locations []LocationState `json:"locations"`
}

type LocationState struct {
	Name      string            `json:"name"`
	RoomID    string            `json:"room_id"`
	Key       string            `json:"key"`
	URI       string            `json:"uri"`
	Carrier   string            `json:"carrier"`
	Transport string            `json:"transport"`
	Payload   map[string]string `json:"payload"`
	Link      string            `json:"link"`
	DNS       string            `json:"dns"`
	Proxy     Socks5Proxy       `json:"proxy,omitempty"`
	Running   bool              `json:"running"`
	Runtime   RuntimeState      `json:"runtime"`
}

type RuntimeState struct {
	Status      string   `json:"status"`
	Running     bool     `json:"running"`
	PID         int      `json:"pid,omitempty"`
	MemoryBytes uint64   `json:"memory_bytes,omitempty"`
	StartedAt   string   `json:"started_at,omitempty"`
	ExitedAt    string   `json:"exited_at,omitempty"`
	ExitError   string   `json:"exit_error,omitempty"`
	LogCount    int      `json:"log_count"`
	Restarts    int      `json:"restarts"`
	PeerCount   *int     `json:"peer_count,omitempty"`
	PeerDevices []string `json:"peer_devices,omitempty"`
}

type LogLine struct {
	Time   string `json:"time"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

type addClientRequest struct {
	ClientID   string            `json:"client_id"`
	FromClient string            `json:"from_client"`
	Refresh    string            `json:"refresh"`
	Quota      Quota             `json:"quota"`
	Locations  []locationRequest `json:"locations"`
	RoomID     string            `json:"room_id"`
	Key        string            `json:"key"`
	Carrier    string            `json:"carrier"`
	Transport  string            `json:"transport"`
	Payload    map[string]string `json:"payload"`
	DNS        string            `json:"dns"`
	Proxy      Socks5Proxy       `json:"proxy"`
	Name       string            `json:"name"`
}

type updateClientRequest struct {
	ClientID  string            `json:"client_id"`
	Refresh   string            `json:"refresh"`
	Quota     Quota             `json:"quota"`
	Locations []locationRequest `json:"locations"`
	RoomID    string            `json:"room_id"`
	Key       string            `json:"key"`
	Carrier   string            `json:"carrier"`
	Transport string            `json:"transport"`
	Payload   map[string]string `json:"payload"`
	DNS       string            `json:"dns"`
	Proxy     Socks5Proxy       `json:"proxy"`
	Name      string            `json:"name"`
}

type locationRequest struct {
	Name      string            `json:"name"`
	RoomID    string            `json:"room_id"`
	Key       string            `json:"key"`
	Carrier   string            `json:"carrier"`
	Transport string            `json:"transport"`
	Payload   map[string]string `json:"payload"`
	DNS       string            `json:"dns"`
	Proxy     Socks5Proxy       `json:"proxy"`
}

type locationActionRequest struct {
	ClientID  string `json:"client_id"`
	RoomID    string `json:"room_id"`
	Transport string `json:"transport"`
}

type clientActionRequest struct {
	ClientID string `json:"client_id"`
}

type generateRoomRequest struct {
	Carrier string `json:"carrier"`
	DNS     string `json:"dns"`
}

type settingsResponse struct {
	Name                string `json:"name"`
	Port                int    `json:"port"`
	SubscriptionPath    string `json:"subscription_path"`
	Refresh             string `json:"refresh,omitempty"`
	AdminUser           string `json:"admin_user"`
	PortOverride        bool   `json:"port_override"`
	RestartRequired     bool   `json:"restart_required,omitempty"`
	SubscriptionBaseURL string `json:"subscription_base_url"`
}

type updateSettingsRequest struct {
	Name             string `json:"name"`
	Port             int    `json:"port"`
	SubscriptionPath string `json:"subscription_path"`
	Refresh          string `json:"refresh"`
}

func settingsHandler(configPath string, supervisor *Supervisor, portOverride bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			cfg, err := loadConfig(configPath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, settingsFromConfig(r, configPath, cfg, portOverride, false))
		case http.MethodPut:
			var req updateSettingsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			cfg, restartRequired, err := updateSettings(configPath, req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			supervisor.UpdateSettings(cfg)
			writeJSON(w, settingsFromConfig(r, configPath, cfg, portOverride, restartRequired))
		default:
			w.Header().Set("Allow", "GET, PUT")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func updateSettings(configPath string, req updateSettingsRequest) (Config, bool, error) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return Config{}, false, err
	}
	oldPort := cfg.Port
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return Config{}, false, errors.New("name is required")
	}
	cfg.Name = req.Name
	cfg.Port = req.Port
	path, err := normalizeSubscriptionPath(req.SubscriptionPath)
	if err != nil {
		return Config{}, false, err
	}
	cfg.SubscriptionPath = path
	cfg.Refresh = strings.TrimSpace(req.Refresh)
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return Config{}, false, err
	}
	if err := saveConfig(configPath, cfg); err != nil {
		return Config{}, false, err
	}
	return cfg, cfg.Port != oldPort, nil
}

func settingsFromConfig(r *http.Request, configPath string, cfg Config, portOverride bool, restartRequired bool) settingsResponse {
	return settingsResponse{
		Name:                cfg.Name,
		Port:                cfg.Port,
		SubscriptionPath:    cfg.SubscriptionPath,
		Refresh:             cfg.Refresh,
		AdminUser:           currentAdminUser(configPath),
		PortOverride:        portOverride,
		RestartRequired:     restartRequired,
		SubscriptionBaseURL: subscriptionBaseURL(r, cfg.SubscriptionPath),
	}
}

func subscriptionBaseURL(r *http.Request, subscriptionPath string) string {
	base := requestOrigin(r)
	if subscriptionPath == "" {
		return base + "/"
	}
	return base + "/" + subscriptionPath + "/"
}

func logRequestTarget(r *http.Request) (string, string, string) {
	query := r.URL.Query()
	if query.Has("client_id") || query.Has("room_id") || query.Has("transport") {
		return strings.TrimSpace(query.Get("client_id")),
			strings.TrimSpace(query.Get("room_id")),
			strings.TrimSpace(query.Get("transport"))
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/logs/"), "/")
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = strings.Split(forwarded, ",")[0]
	}
	host := r.Host
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = strings.Split(forwarded, ",")[0]
	}
	return scheme + "://" + strings.TrimSpace(host)
}

func addClientFromRequest(ctx context.Context, configPath, olcrtcPath string, r *http.Request) (string, error) {
	_ = ctx
	_ = olcrtcPath
	var req addClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return "", fmt.Errorf("parse request: %w", err)
	}
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.FromClient = strings.TrimSpace(req.FromClient)
	req.Refresh = strings.TrimSpace(req.Refresh)
	req.Quota = normalizeQuota(req.Quota)
	if req.ClientID == "" {
		return "", errors.New("client_id is required")
	}
	if strings.Contains(req.ClientID, "/") {
		return "", errors.New("client_id must not contain slash")
	}
	if err := validateQuota(req.Quota); err != nil {
		return "", err
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return "", err
	}
	cfg.ensureClientsFormat()
	for _, client := range cfg.Clients {
		if client.ClientID == req.ClientID {
			return "", fmt.Errorf("client %q already exists", req.ClientID)
		}
	}

	locations, err := createLocationsFromRequest(cfg, req)
	if err != nil {
		return "", err
	}
	for i := range locations {
		locations[i].ClientID = req.ClientID
	}

	cfg.Clients = append(cfg.Clients, Client{ClientID: req.ClientID, Refresh: req.Refresh, Quota: req.Quota, Locations: locations})
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if err := saveConfig(configPath, cfg); err != nil {
		return "", err
	}
	return req.ClientID, nil
}

func updateClientFromRequest(ctx context.Context, configPath, olcrtcPath, clientID string, r *http.Request) error {
	_ = ctx
	_ = olcrtcPath
	var req updateClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.Refresh = strings.TrimSpace(req.Refresh)
	req.Quota = normalizeQuota(req.Quota)
	if err := validateQuota(req.Quota); err != nil {
		return err
	}
	nextClientID := clientID
	if req.ClientID != "" {
		nextClientID = req.ClientID
	}
	if strings.Contains(nextClientID, "/") {
		return errors.New("client_id must not contain slash")
	}

	var locations []Location
	if updateRequestHasLocations(req) {
		var err error
		locations, err = locationsFromUpdateRequest(nextClientID, req)
		if err != nil {
			return err
		}
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	cfg.ensureClientsFormat()

	for i := range cfg.Clients {
		if cfg.Clients[i].ClientID != clientID {
			continue
		}
		if nextClientID != clientID {
			for _, client := range cfg.Clients {
				if client.ClientID == nextClientID {
					return fmt.Errorf("client %q already exists", nextClientID)
				}
			}
			cfg.Clients[i].ClientID = nextClientID
			for j := range cfg.Clients[i].Locations {
				cfg.Clients[i].Locations[j].ClientID = nextClientID
			}
		}
		cfg.Clients[i].Refresh = req.Refresh
		cfg.Clients[i].Quota = req.Quota
		if locations != nil {
			cfg.Clients[i].Locations = locations
		}

		cfg.Normalize()
		if err := cfg.Validate(); err != nil {
			return err
		}
		return saveConfig(configPath, cfg)
	}
	return fmt.Errorf("client %q not found", clientID)
}

func updateRequestHasLocations(req updateClientRequest) bool {
	return len(req.Locations) > 0 ||
		req.RoomID != "" ||
		req.Key != "" ||
		req.Carrier != "" ||
		req.Transport != "" ||
		req.DNS != "" ||
		req.Name != "" ||
		req.Proxy != (Socks5Proxy{}) ||
		len(req.Payload) > 0
}

func createLocationsFromRequest(cfg Config, req addClientRequest) ([]Location, error) {
	if len(req.Locations) > 0 {
		return buildLocations(req.ClientID, req.Locations)
	}
	if req.RoomID != "" || req.Key != "" || req.Carrier != "" || req.Transport != "" || req.DNS != "" || req.Name != "" || req.Proxy != (Socks5Proxy{}) {
		return buildLocations(req.ClientID, []locationRequest{{
			Name:      req.Name,
			RoomID:    req.RoomID,
			Key:       req.Key,
			Carrier:   req.Carrier,
			Transport: req.Transport,
			Payload:   req.Payload,
			DNS:       req.DNS,
			Proxy:     req.Proxy,
		}})
	}
	return templateLocations(cfg, req.FromClient)
}

func locationsFromUpdateRequest(clientID string, req updateClientRequest) ([]Location, error) {
	if len(req.Locations) > 0 {
		return buildLocations(clientID, req.Locations)
	}
	return buildLocations(clientID, []locationRequest{{
		Name:      req.Name,
		RoomID:    req.RoomID,
		Key:       req.Key,
		Carrier:   req.Carrier,
		Transport: req.Transport,
		Payload:   req.Payload,
		DNS:       req.DNS,
		Proxy:     req.Proxy,
	}})
}

func buildLocations(clientID string, requests []locationRequest) ([]Location, error) {
	if len(requests) == 0 {
		return nil, errors.New("locations must not be empty")
	}
	locations := make([]Location, 0, len(requests))
	seen := make(map[string]struct{}, len(requests))
	for i, req := range requests {
		req.Name = strings.TrimSpace(req.Name)
		req.RoomID = strings.TrimSpace(req.RoomID)
		req.Key = strings.TrimSpace(req.Key)
		req.Carrier = strings.TrimSpace(req.Carrier)
		req.Transport = strings.TrimSpace(req.Transport)
		req.Payload = cleanPayload(req.Payload)
		req.DNS = strings.TrimSpace(req.DNS)
		req.Proxy = normalizeProxy(req.Proxy)

		prefix := fmt.Sprintf("locations[%d]", i)
		if req.RoomID == "" || req.RoomID == "any" {
			return nil, fmt.Errorf("%s.room_id must be concrete", prefix)
		}
		if err := validateRequestKey(req.Key); err != nil {
			return nil, fmt.Errorf("%s.key: %w", prefix, err)
		}
		carrier := defaultString(req.Carrier, "wbstream")
		transport := defaultString(req.Transport, "datachannel")
		dns := defaultString(req.DNS, "1.1.1.1:53")
		transportConfig := Transport{Type: transport, Payload: req.Payload}
		if !isSupported(carrier, transport) {
			return nil, fmt.Errorf("unsupported carrier/transport combination %s + %s", carrier, transport)
		}
		if err := validatePayload(transportConfig); err != nil {
			return nil, fmt.Errorf("%s.transport: %w", prefix, err)
		}
		if err := validateProxy(req.Proxy); err != nil {
			return nil, fmt.Errorf("%s.proxy: %w", prefix, err)
		}
		name := req.Name
		if name == "" {
			name = "Default location"
		}
		loc := Location{
			Name:      name,
			ClientID:  clientID,
			Endpoint:  Endpoint{RoomID: req.RoomID, Key: req.Key},
			Carrier:   carrier,
			Transport: transportConfig,
			Link:      "direct",
			Data:      "data",
			DNS:       dns,
			Proxy:     req.Proxy,
		}
		key := locationKey(loc)
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("%s location key %q is duplicated", prefix, key)
		}
		seen[key] = struct{}{}
		locations = append(locations, loc)
	}
	return locations, nil
}

func validateRequestKey(key string) error {
	if key == "" {
		return errors.New("is required")
	}
	if len(key) != 64 {
		return errors.New("must be 64 hex characters")
	}
	if _, err := hex.DecodeString(key); err != nil {
		return errors.New("must be 64 hex characters")
	}
	return nil
}

func normalizeProxy(proxy Socks5Proxy) Socks5Proxy {
	proxy.Addr = strings.TrimSpace(proxy.Addr)
	proxy.User = strings.TrimSpace(proxy.User)
	return proxy
}

func validateProxy(proxy Socks5Proxy) error {
	if proxy.Addr == "" {
		if proxy.Port != 0 {
			return errors.New("addr is required when port is set")
		}
		if proxy.User != "" || proxy.Pass != "" {
			return errors.New("addr is required when credentials are set")
		}
		return nil
	}
	if proxy.Port <= 0 || proxy.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if len(proxy.User) > 255 {
		return errors.New("user must be at most 255 bytes")
	}
	if len(proxy.Pass) > 255 {
		return errors.New("pass must be at most 255 bytes")
	}
	return nil
}

func deleteClient(configPath, clientID string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	cfg.ensureClientsFormat()

	next := cfg.Clients[:0]
	deleted := false
	for _, client := range cfg.Clients {
		if client.ClientID == clientID {
			deleted = true
			continue
		}
		next = append(next, client)
	}
	if !deleted {
		return fmt.Errorf("client %q not found", clientID)
	}
	cfg.Clients = next
	if len(cfg.Clients) == 0 {
		cfg.Locations = nil
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	return saveConfig(configPath, cfg)
}

func addLocationFromRequest(ctx context.Context, configPath, olcrtcPath, clientID string, r *http.Request) error {
	_ = ctx
	_ = olcrtcPath
	clientID = strings.TrimSpace(clientID)
	var req addClientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	req.ClientID = clientID
	req.Carrier = strings.TrimSpace(req.Carrier)
	req.Transport = strings.TrimSpace(req.Transport)
	req.Payload = cleanPayload(req.Payload)
	req.DNS = strings.TrimSpace(req.DNS)
	req.Name = strings.TrimSpace(req.Name)
	req.Proxy = normalizeProxy(req.Proxy)
	locs, err := createLocationsFromRequest(Config{}, req)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	cfg.ensureClientsFormat()
	for i := range cfg.Clients {
		if cfg.Clients[i].ClientID == clientID {
			cfg.Clients[i].Locations = append(cfg.Clients[i].Locations, locs...)
			cfg.Normalize()
			if err := cfg.Validate(); err != nil {
				return err
			}
			return saveConfig(configPath, cfg)
		}
	}
	return fmt.Errorf("client %q not found", clientID)
}

func deleteLocation(configPath, clientID, roomID string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	cfg.ensureClientsFormat()
	for i := range cfg.Clients {
		if cfg.Clients[i].ClientID != clientID {
			continue
		}
		next := cfg.Clients[i].Locations[:0]
		deleted := false
		for _, loc := range cfg.Clients[i].Locations {
			if loc.Endpoint.RoomID == roomID {
				deleted = true
				continue
			}
			next = append(next, loc)
		}
		if !deleted {
			return fmt.Errorf("location %q not found", roomID)
		}
		cfg.Clients[i].Locations = next
		cfg.Normalize()
		if err := cfg.Validate(); err != nil {
			return err
		}
		return saveConfig(configPath, cfg)
	}
	return fmt.Errorf("client %q not found", clientID)
}

func regenerateClientRoom(ctx context.Context, configPath, olcrtcPath, clientID string) error {
	_ = ctx
	_ = olcrtcPath
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return errors.New("client_id is required")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	cfg.ensureClientsFormat()
	for i := range cfg.Clients {
		if cfg.Clients[i].ClientID != clientID {
			continue
		}
		for j := range cfg.Clients[i].Locations {
			loc := &cfg.Clients[i].Locations[j]
			loc.Endpoint.RoomID, err = generateRoomIDForCarrier(loc.Carrier, loc.Endpoint.RoomID)
			if err != nil {
				return err
			}
		}
		cfg.Normalize()
		if err := cfg.Validate(); err != nil {
			return err
		}
		return saveConfig(configPath, cfg)
	}
	return fmt.Errorf("client %q not found", clientID)
}

func rotateClientKey(configPath, clientID string) error {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return errors.New("client_id is required")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	cfg.ensureClientsFormat()
	for i := range cfg.Clients {
		if cfg.Clients[i].ClientID != clientID {
			continue
		}
		for j := range cfg.Clients[i].Locations {
			cfg.Clients[i].Locations[j].Endpoint.Key, err = randomHex(32)
			if err != nil {
				return err
			}
		}
		cfg.Normalize()
		if err := cfg.Validate(); err != nil {
			return err
		}
		return saveConfig(configPath, cfg)
	}
	return fmt.Errorf("client %q not found", clientID)
}

func (c *Config) ensureClientsFormat() {
	for i := range c.Clients {
		for j := range c.Clients[i].Locations {
			if c.Clients[i].Locations[j].ClientID == "" {
				c.Clients[i].Locations[j].ClientID = c.Clients[i].ClientID
			}
		}
	}
}

func templateLocations(cfg Config, fromClient string) ([]Location, error) {
	if fromClient == "" && len(cfg.Clients) > 0 {
		fromClient = cfg.Clients[0].ClientID
	}
	for _, client := range cfg.Clients {
		if client.ClientID != fromClient {
			continue
		}
		if len(client.Locations) == 0 {
			return nil, fmt.Errorf("client %q has no locations", fromClient)
		}
		locations := make([]Location, len(client.Locations))
		copy(locations, client.Locations)
		return locations, nil
	}
	return nil, fmt.Errorf("template client %q not found", fromClient)
}

func generateRoomID(ctx context.Context, olcrtcPath, carrier, dns string) (string, error) {
	_ = ctx
	_ = olcrtcPath
	_ = dns
	return generateRoomIDForCarrier(carrier, "")
}

func generateRoomIDForCarrier(carrier, currentRoomID string) (string, error) {
	switch strings.TrimSpace(carrier) {
	case "jitsi":
		room, err := randomUUID()
		if err != nil {
			return "", err
		}
		return jitsiRoomBase(currentRoomID) + "/" + room, nil
	case "wbstream", "telemost":
		return "", fmt.Errorf("%s room generation is not supported by current olcrtc; create a room manually and paste its id", carrier)
	default:
		return "", fmt.Errorf("unknown carrier %q", carrier)
	}
}

func jitsiRoomBase(roomID string) string {
	trimmed := strings.TrimSpace(roomID)
	if idx := strings.LastIndex(trimmed, "/"); idx > 0 {
		base := strings.TrimRight(trimmed[:idx], "/")
		if base != "" {
			return base
		}
	}
	return defaultGeneratedJitsiBase
}

func randomUUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buf)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}

func serverConfig(loc Location) (olcrtcRuntimeConfig, error) {
	cfg := olcrtcRuntimeConfig{
		Mode:   "srv",
		Auth:   olcrtcAuthConfig{Provider: loc.Carrier},
		Room:   olcrtcRoomConfig{ID: loc.Endpoint.RoomID},
		Crypto: olcrtcCryptoConfig{Key: loc.Endpoint.Key},
		Net: olcrtcNetConfig{
			Transport: loc.Transport.Type,
			DNS:       loc.DNS,
		},
		Data: loc.Data,
	}
	if loc.Proxy.Addr != "" {
		cfg.SOCKS = olcrtcSocksConfig{
			ProxyAddr: loc.Proxy.Addr,
			ProxyPort: loc.Proxy.Port,
			ProxyUser: loc.Proxy.User,
			ProxyPass: loc.Proxy.Pass,
		}
	}
	if err := applyTransportPayload(&cfg, loc.Transport); err != nil {
		return olcrtcRuntimeConfig{}, err
	}
	return cfg, nil
}

func applyTransportPayload(cfg *olcrtcRuntimeConfig, transport Transport) error {
	payload := cleanPayload(transport.Payload)
	switch transport.Type {
	case "datachannel":
		return nil
	case "vp8channel":
		vp8 := olcrtcVP8Config{}
		if err := setPayloadInt(payload, "vp8-fps", &vp8.FPS); err != nil {
			return err
		}
		if err := setPayloadInt(payload, "vp8-batch", &vp8.BatchSize); err != nil {
			return err
		}
		if vp8 != (olcrtcVP8Config{}) {
			cfg.VP8 = &vp8
		}
	case "seichannel":
		sei := olcrtcSEIConfig{}
		if err := setPayloadInt(payload, "fps", &sei.FPS); err != nil {
			return err
		}
		if err := setPayloadInt(payload, "batch", &sei.BatchSize); err != nil {
			return err
		}
		if err := setPayloadInt(payload, "frag", &sei.FragmentSize); err != nil {
			return err
		}
		if err := setPayloadInt(payload, "ack-ms", &sei.AckTimeoutMS); err != nil {
			return err
		}
		if sei != (olcrtcSEIConfig{}) {
			cfg.SEI = &sei
		}
	case "videochannel":
		video := olcrtcVideoConfig{}
		if err := setPayloadInt(payload, "video-w", &video.Width); err != nil {
			return err
		}
		if err := setPayloadInt(payload, "video-h", &video.Height); err != nil {
			return err
		}
		if err := setPayloadInt(payload, "video-fps", &video.FPS); err != nil {
			return err
		}
		if err := setPayloadNonNegativeInt(payload, "video-qr-size", &video.QRSize); err != nil {
			return err
		}
		if err := setPayloadInt(payload, "video-tile-module", &video.TileModule); err != nil {
			return err
		}
		if err := setPayloadNonNegativeInt(payload, "video-tile-rs", &video.TileRS); err != nil {
			return err
		}
		video.Bitrate = payload["video-bitrate"]
		video.HW = payload["video-hw"]
		video.Codec = payload["video-codec"]
		video.QRRecovery = payload["video-qr-recovery"]
		if video != (olcrtcVideoConfig{}) {
			cfg.Video = &video
		}
	default:
		return fmt.Errorf("unknown transport %q", transport.Type)
	}
	return nil
}

func setPayloadInt(payload map[string]string, key string, dst *int) error {
	value := strings.TrimSpace(payload[key])
	if value == "" {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("%s must be a positive integer", key)
	}
	*dst = parsed
	return nil
}

func setPayloadNonNegativeInt(payload map[string]string, key string, dst *int) error {
	value := strings.TrimSpace(payload[key])
	if value == "" {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fmt.Errorf("%s must be a non-negative integer", key)
	}
	*dst = parsed
	return nil
}

func writeTempOlcrtcConfig(prefix string, cfg olcrtcRuntimeConfig) (string, error) {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal olcrtc config: %w", err)
	}
	file, err := os.CreateTemp("", prefix+"-*.yaml")
	if err != nil {
		return "", fmt.Errorf("create olcrtc config: %w", err)
	}
	path := file.Name()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write olcrtc config: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close olcrtc config: %w", err)
	}
	return path, nil
}

func randomHex(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	const hex = "0123456789abcdef"
	out := make([]byte, len(buf)*2)
	for i, b := range buf {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out), nil
}

func cleanPayload(payload map[string]string) map[string]string {
	if len(payload) == 0 {
		return nil
	}
	cleaned := make(map[string]string)
	for key, value := range payload {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			continue
		}
		cleaned[key] = value
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func normalizeQuota(q Quota) Quota {
	q.ExpiresAt = strings.TrimSpace(q.ExpiresAt)
	if q.UsedBytes == 0 && q.UsedGB > 0 {
		q.UsedBytes = uint64(q.UsedGB) * 1024 * 1024 * 1024
	}
	if q.UsedBytes > 0 {
		q.UsedGB = int(q.UsedBytes / (1024 * 1024 * 1024))
	}
	return q
}

func validateQuota(q Quota) error {
	if q.SpeedMbps < 0 {
		return errors.New("quota.speed_mbps must be non-negative")
	}
	if q.TrafficGB < 0 {
		return errors.New("quota.traffic_gb must be non-negative")
	}
	if q.UsedGB < 0 {
		return errors.New("quota.used_gb must be non-negative")
	}
	if q.ExpiresAt != "" {
		if _, err := time.Parse("2006-01-02", q.ExpiresAt); err != nil {
			return errors.New("quota.expires_at must use YYYY-MM-DD")
		}
	}
	return nil
}

func quotaStatus(q Quota, now time.Time) string {
	if q.ExpiresAt != "" {
		expires, err := time.Parse("2006-01-02", q.ExpiresAt)
		if err == nil && now.After(expires.Add(24*time.Hour-time.Nanosecond)) {
			return "expired"
		}
	}
	if q.TrafficGB > 0 && quotaUsedBytes(q) >= quotaTrafficBytes(q) {
		return "traffic_exceeded"
	}
	return "active"
}

func quotaUsedBytes(q Quota) uint64 {
	if q.UsedBytes > 0 {
		return q.UsedBytes
	}
	if q.UsedGB > 0 {
		return uint64(q.UsedGB) * 1024 * 1024 * 1024
	}
	return 0
}

func quotaTrafficBytes(q Quota) uint64 {
	if q.TrafficGB <= 0 {
		return 0
	}
	return uint64(q.TrafficGB) * 1024 * 1024 * 1024
}

func activeLocations(cfg Config, now time.Time) []Location {
	quotas := make(map[string]Quota, len(cfg.Clients))
	for _, client := range cfg.Clients {
		quotas[client.ClientID] = client.Quota
	}
	out := make([]Location, 0, len(cfg.Locations))
	for _, loc := range cfg.Locations {
		if quotaStatus(quotas[loc.ClientID], now) != "active" {
			continue
		}
		out = append(out, loc)
	}
	return out
}

type logBuffer struct {
	mu    sync.RWMutex
	lines []LogLine
	next  int
	full  bool
}

func newLogBuffer(size int) *logBuffer {
	return &logBuffer{lines: make([]LogLine, size)}
}

func (b *logBuffer) Append(stream, line string) {
	if b == nil || len(b.lines) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines[b.next] = LogLine{
		Time:   time.Now().UTC().Format(time.RFC3339),
		Stream: stream,
		Line:   line,
	}
	b.next = (b.next + 1) % len(b.lines)
	if b.next == 0 {
		b.full = true
	}
}

func (b *logBuffer) Snapshot() []LogLine {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.full {
		return append([]LogLine(nil), b.lines[:b.next]...)
	}
	out := make([]LogLine, 0, len(b.lines))
	out = append(out, b.lines[b.next:]...)
	out = append(out, b.lines[:b.next]...)
	return out
}

func (b *logBuffer) Count() int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.full {
		return len(b.lines)
	}
	return b.next
}

func (b *logBuffer) PeerSummary() (int, []string, bool) {
	lines := b.Snapshot()
	for i := len(lines) - 1; i >= 0; i-- {
		if count, devices, ok := parsePeerSummaryLine(lines[i].Line); ok {
			return count, devices, true
		}
	}
	return 0, nil, false
}

func parsePeerSummaryLine(line string) (int, []string, bool) {
	const countMarker = "Current peers count:"
	idx := strings.Index(line, countMarker)
	if idx < 0 {
		return 0, nil, false
	}
	rest := strings.TrimSpace(line[idx+len(countMarker):])
	countText, tail, ok := strings.Cut(rest, ",")
	if !ok {
		return 0, nil, false
	}
	count, err := strconv.Atoi(strings.TrimSpace(countText))
	if err != nil || count < 0 {
		return 0, nil, false
	}

	const devicesMarker = "Devices:"
	idx = strings.Index(tail, devicesMarker)
	if idx < 0 {
		return 0, nil, false
	}
	devicesText := strings.TrimSpace(tail[idx+len(devicesMarker):])
	if !strings.HasPrefix(devicesText, "[") {
		return 0, nil, false
	}
	end := strings.Index(devicesText, "]")
	if end < 0 {
		return 0, nil, false
	}
	devicesText = strings.TrimSpace(devicesText[1:end])
	if devicesText == "" {
		return count, nil, true
	}

	rawDevices := strings.Split(devicesText, ",")
	devices := make([]string, 0, len(rawDevices))
	for _, device := range rawDevices {
		device = strings.TrimSpace(device)
		if device != "" {
			devices = append(devices, device)
		}
	}
	return count, devices, true
}

type logWriter struct {
	stream string
	buffer *logBuffer
}

func (w logWriter) Write(p []byte) (int, error) {
	scanner := bufio.NewScanner(bytes.NewReader(p))
	for scanner.Scan() {
		w.buffer.Append(w.stream, scanner.Text())
	}
	return len(p), nil
}

func (p *process) state() RuntimeState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	state := RuntimeState{
		Status:   "exited",
		Running:  p.running,
		LogCount: p.logs.Count(),
		Restarts: p.restarts,
	}
	if p.running {
		state.Status = "running"
	}
	if !p.started.IsZero() {
		state.StartedAt = p.started.UTC().Format(time.RFC3339)
	}
	if !p.exited.IsZero() {
		state.ExitedAt = p.exited.UTC().Format(time.RFC3339)
	}
	if p.exitErr != "" {
		state.ExitError = p.exitErr
	}
	if count, devices, ok := p.logs.PeerSummary(); ok {
		state.PeerCount = &count
		state.PeerDevices = devices
	}
	if p.cmd != nil && p.cmd.Process != nil && p.running {
		state.PID = p.cmd.Process.Pid
		state.MemoryBytes = processMemoryBytes(state.PID)
	}
	return state
}

func processMemoryBytes(pid int) uint64 {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	return parseProcStatusMemoryBytes(data)
}

func parseProcStatusMemoryBytes(data []byte) uint64 {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func (p *process) markExited(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running = false
	p.exited = time.Now()
	if err != nil {
		p.exitErr = err.Error()
	}
}

type Metrics struct {
	Runtime  string        `json:"runtime"`
	Go       GoMetrics     `json:"go"`
	Memory   MemoryMetrics `json:"memory"`
	Manager  RuntimeState  `json:"manager"`
	Children []ChildMetric `json:"children"`
	Host     HostMetrics   `json:"host"`
}

// HostMetrics reports host-level (VPS) resource usage, independent from the
// olcrtc-manager Go process itself: CPU, RAM, swap, disk, network throughput,
// open socket counts, uptime and the server's own addresses. It is rendered
// as the "hosting statistics" panel at the bottom of the admin UI.
type HostMetrics struct {
	CPU         CPUMetrics     `json:"cpu"`
	Memory      HostMemory     `json:"memory"`
	Swap        HostSwap       `json:"swap"`
	Disk        HostDisk       `json:"disk"`
	Network     NetworkMetrics `json:"network"`
	Connections ConnMetrics    `json:"connections"`
	Uptime      UptimeMetrics  `json:"uptime"`
	Addresses   []string       `json:"addresses"`
}

type CPUMetrics struct {
	UsagePercent float64 `json:"usage_percent"`
	Cores        int     `json:"cores"`
	Threads      int     `json:"threads"`
	ModelName    string  `json:"model_name,omitempty"`
	MHz          float64 `json:"mhz,omitempty"`
}

type HostMemory struct {
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	Percent    float64 `json:"percent"`
}

type HostSwap struct {
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	Percent    float64 `json:"percent"`
}

type HostDisk struct {
	Path       string  `json:"path"`
	TotalBytes uint64  `json:"total_bytes"`
	UsedBytes  uint64  `json:"used_bytes"`
	FreeBytes  uint64  `json:"free_bytes"`
	Percent    float64 `json:"percent"`
}

type NetSample struct {
	Time        string  `json:"time"`
	RxRateBytes float64 `json:"rx_rate_bytes_sec"`
	TxRateBytes float64 `json:"tx_rate_bytes_sec"`
}

type NetworkMetrics struct {
	RxBytesTotal   uint64      `json:"rx_bytes_total"`
	TxBytesTotal   uint64      `json:"tx_bytes_total"`
	RxRateBytesSec float64     `json:"rx_rate_bytes_sec"`
	TxRateBytesSec float64     `json:"tx_rate_bytes_sec"`
	RxPeakBytesSec float64     `json:"rx_peak_bytes_sec"`
	TxPeakBytesSec float64     `json:"tx_peak_bytes_sec"`
	History        []NetSample `json:"history"`
}

type ConnMetrics struct {
	TCP   int `json:"tcp"`
	UDP   int `json:"udp"`
	Total int `json:"total"`
}

type UptimeMetrics struct {
	OSUptimeSeconds      uint64 `json:"os_uptime_seconds"`
	ManagerUptimeSeconds uint64 `json:"manager_uptime_seconds"`
}

// hostMetricsHistoryLimit bounds how many network throughput samples are kept
// in memory for the sparkline on the panel's hosting statistics widget.
const hostMetricsHistoryLimit = 120

// hostMetricsState carries the previous CPU/network samples across polls so
// that /api/metrics can report instantaneous rates instead of raw counters.
type hostMetricsState struct {
	mu sync.Mutex

	haveCPU  bool
	cpuIdle  uint64
	cpuTotal uint64

	haveNet bool
	netTime time.Time
	netRx   uint64
	netTx   uint64
	rxPeak  float64
	txPeak  float64
	history []NetSample
}

var globalHostMetricsState = &hostMetricsState{}

func collectHostMetrics() HostMetrics {
	host := HostMetrics{
		CPU:         readCPUMetrics(),
		Memory:      readHostMemory(),
		Swap:        readHostSwap(),
		Disk:        readHostDisk("/"),
		Network:     readNetworkMetrics(),
		Connections: readConnMetrics(),
		Uptime:      readUptimeMetrics(),
		Addresses:   hostAddresses(),
	}
	return host
}

func readCPUMetrics() CPUMetrics {
	m := CPUMetrics{Threads: runtime.NumCPU(), Cores: runtime.NumCPU()}

	if data, err := os.ReadFile("/proc/stat"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "cpu ") {
				continue
			}
			fields := strings.Fields(line)[1:]
			var total uint64
			var idle uint64
			for i, f := range fields {
				v, err := strconv.ParseUint(f, 10, 64)
				if err != nil {
					continue
				}
				total += v
				if i == 3 || i == 4 { // idle + iowait
					idle += v
				}
			}

			globalHostMetricsState.mu.Lock()
			if globalHostMetricsState.haveCPU {
				deltaTotal := total - globalHostMetricsState.cpuTotal
				deltaIdle := idle - globalHostMetricsState.cpuIdle
				if deltaTotal > 0 {
					busy := float64(deltaTotal-deltaIdle) / float64(deltaTotal) * 100
					if busy < 0 {
						busy = 0
					}
					if busy > 100 {
						busy = 100
					}
					m.UsagePercent = busy
				}
			}
			globalHostMetricsState.cpuTotal = total
			globalHostMetricsState.cpuIdle = idle
			globalHostMetricsState.haveCPU = true
			globalHostMetricsState.mu.Unlock()
			break
		}
	}

	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		physicalIDs := map[string]map[string]bool{}
		currentPhysical := "0"
		for _, line := range strings.Split(string(data), "\n") {
			switch {
			case strings.HasPrefix(line, "physical id"):
				if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
					currentPhysical = strings.TrimSpace(parts[1])
				}
			case strings.HasPrefix(line, "core id"):
				if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
					coreID := strings.TrimSpace(parts[1])
					if physicalIDs[currentPhysical] == nil {
						physicalIDs[currentPhysical] = map[string]bool{}
					}
					physicalIDs[currentPhysical][coreID] = true
				}
			case strings.HasPrefix(line, "model name") && m.ModelName == "":
				if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
					m.ModelName = strings.TrimSpace(parts[1])
				}
			case strings.HasPrefix(line, "cpu MHz") && m.MHz == 0:
				if parts := strings.SplitN(line, ":", 2); len(parts) == 2 {
					if v, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
						m.MHz = v
					}
				}
			}
		}
		if len(physicalIDs) > 0 {
			cores := 0
			for _, coreIDs := range physicalIDs {
				cores += len(coreIDs)
			}
			if cores > 0 {
				m.Cores = cores
			}
		}
	}

	return m
}

func readHostMemory() HostMemory {
	values := readMeminfo()
	total := values["MemTotal"] * 1024
	available := values["MemAvailable"] * 1024
	if available == 0 {
		available = (values["MemFree"] + values["Buffers"] + values["Cached"]) * 1024
	}
	used := uint64(0)
	if total > available {
		used = total - available
	}
	m := HostMemory{TotalBytes: total, UsedBytes: used}
	if total > 0 {
		m.Percent = float64(used) / float64(total) * 100
	}
	return m
}

func readHostSwap() HostSwap {
	values := readMeminfo()
	total := values["SwapTotal"] * 1024
	free := values["SwapFree"] * 1024
	used := uint64(0)
	if total > free {
		used = total - free
	}
	s := HostSwap{TotalBytes: total, UsedBytes: used}
	if total > 0 {
		s.Percent = float64(used) / float64(total) * 100
	}
	return s
}

func readMeminfo() map[string]uint64 {
	values := map[string]uint64{}
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return values
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		values[key] = v
	}
	return values
}

func readHostDisk(path string) HostDisk {
	d := HostDisk{Path: path}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return d
	}
	blockSize := uint64(stat.Bsize)
	d.TotalBytes = stat.Blocks * blockSize
	d.FreeBytes = stat.Bavail * blockSize
	if d.TotalBytes > d.FreeBytes {
		d.UsedBytes = d.TotalBytes - d.FreeBytes
	}
	if d.TotalBytes > 0 {
		d.Percent = float64(d.UsedBytes) / float64(d.TotalBytes) * 100
	}
	return d
}

func readNetworkMetrics() NetworkMetrics {
	var rxTotal, txTotal uint64
	if data, err := os.ReadFile("/proc/net/dev"); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, line := range lines[2:] {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				continue
			}
			iface := strings.TrimSpace(parts[0])
			if iface == "lo" || iface == "" {
				continue
			}
			fields := strings.Fields(parts[1])
			if len(fields) < 16 {
				continue
			}
			rx, err1 := strconv.ParseUint(fields[0], 10, 64)
			tx, err2 := strconv.ParseUint(fields[8], 10, 64)
			if err1 == nil {
				rxTotal += rx
			}
			if err2 == nil {
				txTotal += tx
			}
		}
	}

	now := time.Now()
	net := NetworkMetrics{RxBytesTotal: rxTotal, TxBytesTotal: txTotal}

	globalHostMetricsState.mu.Lock()
	defer globalHostMetricsState.mu.Unlock()

	if globalHostMetricsState.haveNet {
		elapsed := now.Sub(globalHostMetricsState.netTime).Seconds()
		if elapsed > 0 {
			if rxTotal >= globalHostMetricsState.netRx {
				net.RxRateBytesSec = float64(rxTotal-globalHostMetricsState.netRx) / elapsed
			}
			if txTotal >= globalHostMetricsState.netTx {
				net.TxRateBytesSec = float64(txTotal-globalHostMetricsState.netTx) / elapsed
			}
		}
	}

	if net.RxRateBytesSec > globalHostMetricsState.rxPeak {
		globalHostMetricsState.rxPeak = net.RxRateBytesSec
	}
	if net.TxRateBytesSec > globalHostMetricsState.txPeak {
		globalHostMetricsState.txPeak = net.TxRateBytesSec
	}

	globalHostMetricsState.history = append(globalHostMetricsState.history, NetSample{
		Time:        now.UTC().Format(time.RFC3339),
		RxRateBytes: net.RxRateBytesSec,
		TxRateBytes: net.TxRateBytesSec,
	})
	if len(globalHostMetricsState.history) > hostMetricsHistoryLimit {
		globalHostMetricsState.history = globalHostMetricsState.history[len(globalHostMetricsState.history)-hostMetricsHistoryLimit:]
	}

	net.RxPeakBytesSec = globalHostMetricsState.rxPeak
	net.TxPeakBytesSec = globalHostMetricsState.txPeak
	net.History = append([]NetSample(nil), globalHostMetricsState.history...)

	globalHostMetricsState.netTime = now
	globalHostMetricsState.netRx = rxTotal
	globalHostMetricsState.netTx = txTotal
	globalHostMetricsState.haveNet = true

	return net
}

func readConnMetrics() ConnMetrics {
	c := ConnMetrics{}
	c.TCP = countProcNetEntries("/proc/net/tcp") + countProcNetEntries("/proc/net/tcp6")
	c.UDP = countProcNetEntries("/proc/net/udp") + countProcNetEntries("/proc/net/udp6")
	c.Total = c.TCP + c.UDP
	return c
}

func countProcNetEntries(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) <= 1 {
		return 0
	}
	count := 0
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func readUptimeMetrics() UptimeMetrics {
	u := UptimeMetrics{
		ManagerUptimeSeconds: uint64(time.Since(managerStartedAt).Seconds()),
	}
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return u
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return u
	}
	if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
		u.OSUptimeSeconds = uint64(v)
	}
	return u
}

func hostAddresses() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out
}

type GoMetrics struct {
	Version    string `json:"version"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Goroutines int    `json:"goroutines"`
}

type MemoryMetrics struct {
	AllocBytes      uint64 `json:"alloc_bytes"`
	SysBytes        uint64 `json:"sys_bytes"`
	HeapAllocBytes  uint64 `json:"heap_alloc_bytes"`
	HeapInuseBytes  uint64 `json:"heap_inuse_bytes"`
	StackInuseBytes uint64 `json:"stack_inuse_bytes"`
}

type ChildMetric struct {
	ClientID  string       `json:"client_id"`
	RoomID    string       `json:"room_id"`
	Transport string       `json:"transport"`
	Name      string       `json:"name"`
	Runtime   RuntimeState `json:"runtime"`
}

func collectMetrics(supervisor *Supervisor) Metrics {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	metrics := Metrics{
		Runtime: time.Now().UTC().Format(time.RFC3339),
		Go: GoMetrics{
			Version:    runtime.Version(),
			OS:         runtime.GOOS,
			Arch:       runtime.GOARCH,
			Goroutines: runtime.NumGoroutine(),
		},
		Memory: MemoryMetrics{
			AllocBytes:      mem.Alloc,
			SysBytes:        mem.Sys,
			HeapAllocBytes:  mem.HeapAlloc,
			HeapInuseBytes:  mem.HeapInuse,
			StackInuseBytes: mem.StackInuse,
		},
		Manager: RuntimeState{
			Status:    "running",
			Running:   true,
			PID:       os.Getpid(),
			StartedAt: managerStartedAt.UTC().Format(time.RFC3339),
		},
		Host: collectHostMetrics(),
	}

	supervisor.mu.RLock()
	defer supervisor.mu.RUnlock()
	metrics.Children = make([]ChildMetric, 0, len(supervisor.processes))
	for _, p := range supervisor.processes {
		metrics.Children = append(metrics.Children, ChildMetric{
			ClientID:  p.location.ClientID,
			RoomID:    p.location.Endpoint.RoomID,
			Transport: p.location.Transport.Type,
			Name:      p.location.Name,
			Runtime:   p.state(),
		})
	}
	sort.Slice(metrics.Children, func(i, j int) bool {
		return strings.Join([]string{metrics.Children[i].ClientID, metrics.Children[i].RoomID, metrics.Children[i].Transport}, ":") <
			strings.Join([]string{metrics.Children[j].ClientID, metrics.Children[j].RoomID, metrics.Children[j].Transport}, ":")
	})
	return metrics
}

type quotaRule struct {
	ClientID string
	ClassID  uint32
	Cgroup   string
	Last     uint64
	Dev      string
	Iface    string
}

type QuotaEnforcer struct {
	configPath string
	supervisor *Supervisor
	mu         sync.Mutex
	rules      map[string]quotaRule
}

func NewQuotaEnforcer(configPath string, supervisor *Supervisor) *QuotaEnforcer {
	q := &QuotaEnforcer{
		configPath: configPath,
		supervisor: supervisor,
		rules:      make(map[string]quotaRule),
	}
	q.cleanupStale(context.Background())
	return q
}

func (q *QuotaEnforcer) Run(ctx context.Context) {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := q.Collect(ctx); err != nil {
				log.Printf("quota accounting collect failed: %v", err)
			}
			timer.Reset(30 * time.Second)
		}
	}
}

func (q *QuotaEnforcer) Register(loc Location, quota Quota, p *process) error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return errors.New("process is not running")
	}
	key := locationKey(loc)
	classID := quotaClassID(key)
	cgroup := filepath.Join("/sys/fs/cgroup/net_cls,net_prio/olcrtc-manager", quotaSafeName(key))
	dev := defaultRouteInterface(context.Background())
	iface := ""
	if p.netns != nil {
		iface = p.netns.HostIf
	}

	if p.netns != nil {
		last := uint64(0)
		if iface != "" {
			if bytes, err := interfaceTXBytes(iface); err == nil {
				last = bytes
			}
		}
		q.mu.Lock()
		if existing, ok := q.rules[key]; ok && existing.Iface == iface {
			last = existing.Last
		}
		q.rules[key] = quotaRule{ClientID: loc.ClientID, ClassID: classID, Cgroup: cgroup, Dev: dev, Iface: iface, Last: last}
		q.mu.Unlock()
		if quota.SpeedMbps > 0 {
			if err := applyNetnsSpeed(context.Background(), p.netns, quota.SpeedMbps); err != nil {
				log.Printf("speed limit unavailable for %s: %v", key, err)
			}
		} else {
			_ = runCmd(context.Background(), "tc", "qdisc", "del", "dev", p.netns.HostIf, "root")
			_ = runCmd(context.Background(), "ip", "netns", "exec", p.netns.Name, "tc", "qdisc", "del", "dev", p.netns.NsIf, "root")
		}
		return nil
	}

	q.mu.Lock()
	last := uint64(0)
	if existing, ok := q.rules[key]; ok {
		last = existing.Last
	}
	q.rules[key] = quotaRule{ClientID: loc.ClientID, ClassID: classID, Cgroup: cgroup, Dev: dev, Iface: iface, Last: last}
	q.mu.Unlock()

	if err := os.MkdirAll(cgroup, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cgroup, "net_cls.classid"), []byte(strconv.FormatUint(uint64(classID), 10)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(cgroup, "tasks"), []byte(strconv.Itoa(p.cmd.Process.Pid)), 0o644); err != nil {
		return err
	}
	q.deleteRule(context.Background(), "INPUT", classID)
	if err := q.iptables(context.Background(), "-I", "INPUT", "1", "-m", "cgroup", "--cgroup", quotaClassArg(classID), "-m", "comment", "--comment", "olcrtc-manager"); err != nil {
		return err
	}
	if quota.SpeedMbps > 0 && dev != "" && iface == "" {
		if err := q.applySpeedLimit(context.Background(), dev, classID, quota.SpeedMbps); err != nil {
			log.Printf("speed limit unavailable for %s: %v", key, err)
		}
	}
	return nil
}

func (q *QuotaEnforcer) Unregister(key string) {
	q.mu.Lock()
	rule, ok := q.rules[key]
	if ok {
		delete(q.rules, key)
	}
	q.mu.Unlock()
	if !ok {
		return
	}
	q.deleteRule(context.Background(), "INPUT", rule.ClassID)
	if rule.Dev != "" {
		q.deleteSpeedLimit(context.Background(), rule.Dev, rule.ClassID)
	}
	_ = os.Remove(filepath.Join(rule.Cgroup, "tasks"))
	_ = os.Remove(rule.Cgroup)
}

func (q *QuotaEnforcer) Collect(ctx context.Context) error {
	q.mu.Lock()
	rules := make([]quotaRule, 0, len(q.rules))
	for _, rule := range q.rules {
		rules = append(rules, rule)
	}
	q.mu.Unlock()
	if len(rules) == 0 {
		return nil
	}

	deltaByClient := make(map[string]uint64)
	for _, rule := range rules {
		bytes, err := q.ruleBytes(ctx, rule)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if bytes > rule.Last {
			deltaByClient[rule.ClientID] += bytes - rule.Last
		}
		q.updateLast(rule.ClassID, bytes)
	}
	if len(deltaByClient) == 0 {
		return nil
	}

	cfg, err := loadConfig(q.configPath)
	if err != nil {
		return err
	}
	cfg.ensureClientsFormat()
	changed := false
	for i := range cfg.Clients {
		bytes, ok := deltaByClient[cfg.Clients[i].ClientID]
		if !ok {
			continue
		}
		cfg.Clients[i].Quota.UsedBytes = quotaUsedBytes(cfg.Clients[i].Quota) + bytes
		cfg.Clients[i].Quota.UsedGB = int(cfg.Clients[i].Quota.UsedBytes / (1024 * 1024 * 1024))
		changed = true
	}
	if changed {
		cfg.Normalize()
		if err := saveConfigWithoutBackup(q.configPath, cfg); err != nil {
			return err
		}
	}
	if q.supervisor != nil {
		q.supervisor.ApplyQuotaConfig(cfg, time.Now())
	}
	return nil
}

func (q *QuotaEnforcer) updateLast(classID uint32, bytes uint64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for key, rule := range q.rules {
		if rule.ClassID == classID {
			rule.Last = bytes
			q.rules[key] = rule
			return
		}
	}
}

func (q *QuotaEnforcer) ruleBytes(ctx context.Context, rule quotaRule) (uint64, error) {
	if rule.Iface != "" {
		return interfaceTXBytes(rule.Iface)
	}
	inBytes, err := q.chainBytes(ctx, "INPUT", rule.ClassID)
	if err != nil {
		return 0, err
	}
	return inBytes, nil
}

func interfaceTXBytes(iface string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join("/sys/class/net", iface, "statistics", "tx_bytes"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

func (q *QuotaEnforcer) chainBytes(ctx context.Context, chain string, classID uint32) (uint64, error) {
	out, err := q.iptablesOutput(ctx, "-L", chain, "-v", "-n", "-x")
	if err != nil {
		return 0, err
	}
	needle := "cgroup " + strconv.FormatUint(uint64(classID), 10)
	var total uint64
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, needle) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		bytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err == nil {
			total += bytes
		}
	}
	return total, nil
}

func (q *QuotaEnforcer) deleteRule(ctx context.Context, chain string, classID uint32) {
	for i := 0; i < 8; i++ {
		if err := q.iptables(ctx, "-D", chain, "-m", "cgroup", "--cgroup", quotaClassArg(classID), "-m", "comment", "--comment", "olcrtc-manager"); err != nil {
			return
		}
	}
}

func (q *QuotaEnforcer) cleanupStale(ctx context.Context) {
	for _, chain := range []string{"INPUT", "OUTPUT"} {
		for i := 0; i < 64; i++ {
			line, ok := q.firstCgroupRuleLine(ctx, chain)
			if !ok {
				break
			}
			if err := q.iptables(ctx, "-D", chain, strconv.Itoa(line)); err != nil {
				break
			}
		}
	}
	if dev := defaultRouteInterface(ctx); dev != "" {
		_ = q.tc(ctx, "qdisc", "del", "dev", dev, "root")
		_ = q.tc(ctx, "qdisc", "del", "dev", dev, "ingress")
	}
	cleanupManagerNetns(ctx)
	_ = os.RemoveAll("/sys/fs/cgroup/net_cls,net_prio/olcrtc-manager")
}

func cleanupManagerNetns(ctx context.Context) {
	if out, err := exec.CommandContext(ctx, "ip", "netns", "list").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			name := strings.Fields(line)
			if len(name) == 0 || !strings.HasPrefix(name[0], "olc-") {
				continue
			}
			_ = runCmd(ctx, "ip", "netns", "del", name[0])
			_ = os.RemoveAll(filepath.Join("/etc/netns", name[0]))
		}
	}
	if out, err := exec.CommandContext(ctx, "ip", "-o", "link", "show").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			name := strings.TrimSuffix(fields[1], ":")
			name = strings.Split(name, "@")[0]
			if strings.HasPrefix(name, "olh") {
				_ = runCmd(ctx, "ip", "link", "del", name)
			}
		}
	}
}

func (q *QuotaEnforcer) firstCgroupRuleLine(ctx context.Context, chain string) (int, bool) {
	out, err := q.iptablesOutput(ctx, "-L", chain, "-v", "-n", "-x", "--line-numbers")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "cgroup ") || !strings.Contains(line, "olcrtc-manager") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		n, err := strconv.Atoi(fields[0])
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

func (q *QuotaEnforcer) iptables(ctx context.Context, args ...string) error {
	_, err := q.iptablesOutput(ctx, args...)
	return err
}

func (q *QuotaEnforcer) iptablesOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, "iptables", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("iptables %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (q *QuotaEnforcer) applySpeedLimit(ctx context.Context, dev string, classID uint32, speedMbps int) error {
	rate := strconv.Itoa(speedMbps) + "mbit"
	class := tcClassID(classID)
	_ = q.tc(ctx, "qdisc", "add", "dev", dev, "root", "handle", "10:", "htb", "default", "ffff")
	_ = q.tc(ctx, "class", "add", "dev", dev, "parent", "10:", "classid", "10:ffff", "htb", "rate", "10gbit", "ceil", "10gbit")
	_ = q.tc(ctx, "class", "del", "dev", dev, "classid", class)
	if err := q.tc(ctx, "class", "add", "dev", dev, "parent", "10:", "classid", class, "htb", "rate", rate, "ceil", rate); err != nil {
		return err
	}
	q.ensureCgroupFilter(ctx, dev)
	return nil
}

func (q *QuotaEnforcer) deleteSpeedLimit(ctx context.Context, dev string, classID uint32) {
	class := tcClassID(classID)
	_ = q.tc(ctx, "class", "del", "dev", dev, "classid", class)
}

func (q *QuotaEnforcer) ensureCgroupFilter(ctx context.Context, dev string) {
	out, err := q.tcOutput(ctx, "filter", "show", "dev", dev, "parent", "10:")
	if err == nil && strings.Contains(string(out), "cgroup") {
		return
	}
	_ = q.tc(ctx, "filter", "add", "dev", dev, "parent", "10:", "protocol", "ip", "prio", "10", "handle", "1:", "cgroup")
	_ = q.tc(ctx, "filter", "add", "dev", dev, "parent", "10:", "protocol", "ipv6", "prio", "10", "handle", "1:", "cgroup")
}

func (q *QuotaEnforcer) tc(ctx context.Context, args ...string) error {
	_, err := q.tcOutput(ctx, args...)
	return err
}

func (q *QuotaEnforcer) tcOutput(ctx context.Context, args ...string) ([]byte, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, "tc", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("tc %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func defaultRouteInterface(ctx context.Context) string {
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, "ip", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "dev" {
			return fields[i+1]
		}
	}
	return ""
}

func quotaClassID(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return 0x100000 + (h.Sum32() & 0x00ffff)
}

func quotaClassArg(classID uint32) string {
	return fmt.Sprintf("0x%x", classID)
}

func tcClassID(classID uint32) string {
	return fmt.Sprintf("%x:%x", classID>>16, classID&0xffff)
}

func quotaSafeName(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 96 {
			break
		}
	}
	if b.Len() == 0 {
		return "location"
	}
	return b.String()
}

func saveConfig(path string, cfg Config) error {
	backupConfig(path)
	if err := writeConfig(path, cfg); err != nil {
		return err
	}
	appendAudit(path, "config_saved", "")
	return nil
}

func saveConfigWithoutBackup(path string, cfg Config) error {
	return writeConfig(path, cfg)
}

func writeConfig(path string, cfg Config) error {
	cfg.Normalize()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func backupConfig(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	dir := filepath.Join(filepath.Dir(path), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	name := "config-" + time.Now().UTC().Format("20060102-150405") + ".json"
	_ = os.WriteFile(filepath.Join(dir, name), data, 0o600)
}

func appendAudit(configPath, action, detail string) {
	entry := map[string]string{
		"time":   time.Now().UTC().Format(time.RFC3339),
		"action": action,
		"detail": detail,
	}
	data, _ := json.Marshal(entry)
	path := filepath.Join(filepath.Dir(configPath), "audit.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

func readAudit(configPath string, limit int) []map[string]string {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), "audit.log"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if limit <= 0 || limit > len(lines) {
		limit = len(lines)
	}
	out := make([]map[string]string, 0, limit)
	for i := len(lines) - limit; i < len(lines); i++ {
		var entry map[string]string
		if json.Unmarshal([]byte(lines[i]), &entry) == nil {
			out = append(out, entry)
		}
	}
	return out
}

func (s *Supervisor) stopLocked(id string) {
	p, ok := s.processes[id]
	if !ok {
		return
	}
	if s.quota != nil {
		s.quota.Unregister(id)
	}
	stopProcess(p)
	delete(s.processes, id)
}

func locationsByKey(locations []Location) map[string]Location {
	byKey := make(map[string]Location, len(locations))
	for _, loc := range locations {
		byKey[locationKey(loc)] = loc
	}
	return byKey
}

func stopProcessMap(processes map[string]*process) {
	for _, p := range processes {
		stopProcess(p)
	}
}

func waitProcessStopped(ctx context.Context, p *process, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !p.state().Running {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for olcrtc to stop")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

type netnsRuntime struct {
	Name   string
	HostIf string
	NsIf   string
	HostIP string
	NsIP   string
	Dev    string
}

func setupNetns(ctx context.Context, loc Location) (*netnsRuntime, error) {
	key := locationKey(loc)
	token := fmt.Sprintf("%08x", quotaClassID(key)&0xffffffff)
	suffix, err := randomHex(2)
	if err != nil {
		return nil, err
	}
	ns := &netnsRuntime{
		Name:   "olc-" + token + "-" + suffix,
		HostIf: "olh" + token + suffix,
		NsIf:   "oln" + token + suffix,
		Dev:    defaultRouteInterface(ctx),
	}
	hostIP, nsIP := netnsIPs(key)
	ns.HostIP = hostIP
	ns.NsIP = nsIP
	if ns.Dev == "" {
		return nil, errors.New("default route interface not found")
	}

	cleanupNetns(ctx, ns)
	if err := runCmd(ctx, "ip", "netns", "add", ns.Name); err != nil {
		return nil, err
	}
	if err := writeNetnsResolv(ns.Name, loc.DNS); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	if err := runCmd(ctx, "ip", "link", "add", ns.HostIf, "type", "veth", "peer", "name", ns.NsIf); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	if err := runCmd(ctx, "ip", "link", "set", ns.NsIf, "netns", ns.Name); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	if err := runCmd(ctx, "ip", "addr", "add", ns.HostIP+"/30", "dev", ns.HostIf); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	if err := runCmd(ctx, "ip", "link", "set", ns.HostIf, "up"); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	if err := runCmd(ctx, "ip", "netns", "exec", ns.Name, "ip", "addr", "add", ns.NsIP+"/30", "dev", ns.NsIf); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	if err := runCmd(ctx, "ip", "netns", "exec", ns.Name, "ip", "link", "set", "lo", "up"); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	if err := runCmd(ctx, "ip", "netns", "exec", ns.Name, "ip", "link", "set", ns.NsIf, "up"); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	if err := runCmd(ctx, "ip", "netns", "exec", ns.Name, "ip", "route", "add", "default", "via", ns.HostIP); err != nil {
		cleanupNetns(ctx, ns)
		return nil, err
	}
	_ = runCmd(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1")
	addNetnsFirewall(ctx, ns)

	quota := quotaForClientConfigPath(loc.ClientID)
	if quota.SpeedMbps > 0 {
		if err := applyNetnsSpeed(ctx, ns, quota.SpeedMbps); err != nil {
			log.Printf("speed limit unavailable for %s: %v", locationKey(loc), err)
		}
	}
	return ns, nil
}

func quotaForClientConfigPath(clientID string) Quota {
	if adminConfigPath == "" {
		return Quota{}
	}
	cfg, err := loadConfig(adminConfigPath)
	if err != nil {
		return Quota{}
	}
	cfg.ensureClientsFormat()
	return quotaForClient(cfg, clientID)
}

func cleanupNetns(ctx context.Context, ns *netnsRuntime) {
	if ns == nil {
		return
	}
	delNetnsFirewall(ctx, ns)
	_ = runCmd(ctx, "ip", "link", "del", ns.HostIf)
	_ = runCmd(ctx, "ip", "netns", "del", ns.Name)
	_ = os.RemoveAll(filepath.Join("/etc/netns", ns.Name))
}

func addNetnsFirewall(ctx context.Context, ns *netnsRuntime) {
	delNetnsFirewall(ctx, ns)
	_ = runCmd(ctx, "iptables", "-t", "nat", "-I", "POSTROUTING", "1", "-s", ns.NsIP+"/32", "-o", ns.Dev, "-j", "MASQUERADE", "-m", "comment", "--comment", "olcrtc-manager-netns")
	_ = runCmd(ctx, "iptables", "-I", "FORWARD", "1", "-i", ns.HostIf, "-o", ns.Dev, "-j", "ACCEPT", "-m", "comment", "--comment", "olcrtc-manager-netns")
	_ = runCmd(ctx, "iptables", "-I", "FORWARD", "1", "-i", ns.Dev, "-o", ns.HostIf, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT", "-m", "comment", "--comment", "olcrtc-manager-netns")
}

func delNetnsFirewall(ctx context.Context, ns *netnsRuntime) {
	for i := 0; i < 8; i++ {
		if runCmd(ctx, "iptables", "-t", "nat", "-D", "POSTROUTING", "-s", ns.NsIP+"/32", "-o", ns.Dev, "-j", "MASQUERADE", "-m", "comment", "--comment", "olcrtc-manager-netns") != nil {
			break
		}
	}
	for i := 0; i < 8; i++ {
		if runCmd(ctx, "iptables", "-D", "FORWARD", "-i", ns.HostIf, "-o", ns.Dev, "-j", "ACCEPT", "-m", "comment", "--comment", "olcrtc-manager-netns") != nil {
			break
		}
	}
	for i := 0; i < 8; i++ {
		if runCmd(ctx, "iptables", "-D", "FORWARD", "-i", ns.Dev, "-o", ns.HostIf, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT", "-m", "comment", "--comment", "olcrtc-manager-netns") != nil {
			break
		}
	}
}

func applyNetnsSpeed(ctx context.Context, ns *netnsRuntime, speedMbps int) error {
	rate := strconv.Itoa(speedMbps) + "mbit"
	if err := applyHTBSpeed(ctx, ns.HostIf, rate); err != nil {
		return err
	}
	if err := runCmd(ctx, "ip", "netns", "exec", ns.Name, "tc", "qdisc", "replace", "dev", ns.NsIf, "root", "handle", "1:", "htb", "default", "10"); err != nil {
		return err
	}
	if err := runCmd(ctx, "ip", "netns", "exec", ns.Name, "tc", "class", "replace", "dev", ns.NsIf, "parent", "1:", "classid", "1:10", "htb", "rate", rate, "ceil", rate); err != nil {
		return err
	}
	return nil
}

func applyHTBSpeed(ctx context.Context, dev, rate string) error {
	if err := runCmd(ctx, "tc", "qdisc", "replace", "dev", dev, "root", "handle", "1:", "htb", "default", "10"); err != nil {
		return err
	}
	return runCmd(ctx, "tc", "class", "replace", "dev", dev, "parent", "1:", "classid", "1:10", "htb", "rate", rate, "ceil", rate)
}

func writeNetnsResolv(nsName, dns string) error {
	host := strings.TrimSpace(dns)
	if strings.Contains(host, ":") {
		host, _, _ = net.SplitHostPort(host)
	}
	if net.ParseIP(host) == nil {
		host = "1.1.1.1"
	}
	dir := filepath.Join("/etc/netns", nsName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "resolv.conf"), []byte("nameserver "+host+"\n"), 0o644)
}

func netnsIPs(key string) (string, string) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	n := h.Sum32() % 16000
	second := 200 + int(n/4096)
	third := int(n%4096) / 16
	fourth := 1 + int(n%16)*4
	return fmt.Sprintf("10.%d.%d.%d", second, third, fourth), fmt.Sprintf("10.%d.%d.%d", second, third, fourth+1)
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmdCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func startInstance(ctx context.Context, olcrtcPath string, loc Location) (*process, error) {
	cfg, err := serverConfig(loc)
	if err != nil {
		return nil, fmt.Errorf("build olcrtc config for %s: %w", locationKey(loc), err)
	}
	configPath, err := writeTempOlcrtcConfig("olcrtc-manager-srv", cfg)
	if err != nil {
		return nil, err
	}
	ns, err := setupNetns(ctx, loc)
	if err != nil {
		_ = os.Remove(configPath)
		return nil, fmt.Errorf("setup netns for %s: %w", locationKey(loc), err)
	}

	cmdArgs := []string{"netns", "exec", ns.Name, olcrtcPath, configPath}
	cmd := exec.CommandContext(ctx, "ip", cmdArgs...)
	logs := newLogBuffer(500)
	cmd.Stdout = logWriter{stream: "stdout", buffer: logs}
	cmd.Stderr = logWriter{stream: "stderr", buffer: logs}

	if err := cmd.Start(); err != nil {
		cleanupNetns(context.Background(), ns)
		_ = os.Remove(configPath)
		return nil, fmt.Errorf("start olcrtc for %s: %w", locationKey(loc), err)
	}

	p := &process{location: loc, cmd: cmd, netns: ns, logs: logs, done: make(chan error, 1), started: time.Now(), running: true}
	log.Printf("started olcrtc for %s in %s: %s %s", locationKey(loc), ns.Name, olcrtcPath, configPath)

	go func() {
		err := cmd.Wait()
		p.markExited(err)
		cleanupNetns(context.Background(), ns)
		_ = os.Remove(configPath)
		p.done <- err
	}()

	return p, nil
}

func stopProcess(p *process) {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func startInstances(ctx context.Context, olcrtcPath string, locations []Location) ([]*process, error) {
	processes := make([]*process, 0, len(locations))
	for _, loc := range locations {
		p, err := startInstance(ctx, olcrtcPath, loc)
		if err != nil {
			stopInstances(processes)
			return nil, err
		}
		processes = append(processes, p)
	}
	return processes, nil
}

func stopInstances(processes []*process) {
	for _, p := range processes {
		stopProcess(p)
	}
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.Normalize()
	return cfg, nil
}

func (c *Config) Normalize() {
	if c.Version == 0 && c.LegacyVersion != 0 {
		c.Version = c.LegacyVersion
	}

	if path, err := normalizeSubscriptionPath(c.SubscriptionPath); err == nil {
		c.SubscriptionPath = path
	}
	c.Refresh = strings.TrimSpace(c.Refresh)
	for i := range c.Clients {
		c.Clients[i].Refresh = strings.TrimSpace(c.Clients[i].Refresh)
	}

	if len(c.Clients) == 0 {
		return
	}

	locations := make([]Location, 0)
	for _, client := range c.Clients {
		for _, loc := range client.Locations {
			if loc.ClientID == "" {
				loc.ClientID = client.ClientID
			}
			loc.Proxy = normalizeProxy(loc.Proxy)
			locations = append(locations, loc)
		}
	}
	c.Locations = locations
}

func (c Config) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if _, err := normalizeSubscriptionPath(c.SubscriptionPath); err != nil {
		return fmt.Errorf("subscription_path: %w", err)
	}
	if err := validateRefresh(c.Refresh); err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	for i, client := range c.Clients {
		if err := validateRefresh(client.Refresh); err != nil {
			return fmt.Errorf("clients[%d].refresh: %w", i, err)
		}
		if err := validateQuota(client.Quota); err != nil {
			return fmt.Errorf("clients[%d].quota: %w", i, err)
		}
	}

	ids := make(map[string]struct{}, len(c.Locations))
	for i, loc := range c.Locations {
		prefix := fmt.Sprintf("locations[%d]", i)
		if loc.ClientID == "" {
			return fmt.Errorf("%s.client-id is required", prefix)
		}
		if loc.Endpoint.RoomID == "" || loc.Endpoint.RoomID == "any" {
			return fmt.Errorf("%s.endpoint.room_id must be concrete", prefix)
		}
		if loc.Endpoint.Key == "" {
			return fmt.Errorf("%s.endpoint.key is required", prefix)
		}
		if loc.Carrier == "" {
			return fmt.Errorf("%s.carrier is required", prefix)
		}
		if loc.Transport.Type == "" {
			return fmt.Errorf("%s.transport.type is required", prefix)
		}
		if err := validateProxy(loc.Proxy); err != nil {
			return fmt.Errorf("%s.proxy: %w", prefix, err)
		}
		key := locationKey(loc)
		if _, exists := ids[key]; exists {
			return fmt.Errorf("%s location key %q is duplicated", prefix, key)
		}
		ids[key] = struct{}{}
		if !isSupported(loc.Carrier, loc.Transport.Type) {
			return fmt.Errorf("%s: unsupported carrier/transport combination %s + %s", prefix, loc.Carrier, loc.Transport.Type)
		}
		if err := validatePayload(loc.Transport); err != nil {
			return fmt.Errorf("%s.transport: %w", prefix, err)
		}
		if loc.Link == "" {
			return fmt.Errorf("%s.link is required", prefix)
		}
		if loc.Data == "" {
			return fmt.Errorf("%s.data is required", prefix)
		}
		if loc.DNS == "" {
			return fmt.Errorf("%s.dns is required", prefix)
		}
	}
	return nil
}

func locationKey(loc Location) string {
	return strings.Join([]string{loc.ClientID, loc.Endpoint.RoomID, loc.Transport.Type}, ":")
}

func isSupported(carrier, transport string) bool {
	matrix := map[string]map[string]bool{
		"telemost": {
			"datachannel":  false,
			"vp8channel":   true,
			"seichannel":   false,
			"videochannel": true,
		},
		"wbstream": {
			"datachannel":  false,
			"vp8channel":   true,
			"seichannel":   true,
			"videochannel": true,
		},
		"jitsi": {
			"datachannel":  true,
			"vp8channel":   true,
			"seichannel":   true,
			"videochannel": true,
		},
	}
	return matrix[carrier][transport]
}

func validatePayload(t Transport) error {
	allowed := map[string]map[string]struct{}{
		"datachannel":  {},
		"vp8channel":   {"vp8-fps": {}, "vp8-batch": {}},
		"seichannel":   {"fps": {}, "batch": {}, "frag": {}, "ack-ms": {}},
		"videochannel": {"video-w": {}, "video-h": {}, "video-fps": {}, "video-bitrate": {}, "video-hw": {}, "video-codec": {}, "video-qr-size": {}, "video-qr-recovery": {}, "video-tile-module": {}, "video-tile-rs": {}},
	}

	keys, ok := allowed[t.Type]
	if !ok {
		return fmt.Errorf("unknown transport %q", t.Type)
	}
	for key := range t.Payload {
		if _, ok := keys[key]; !ok {
			return fmt.Errorf("unsupported payload key %q for %s", key, t.Type)
		}
	}
	if _, err := serverConfig(Location{Transport: t}); err != nil {
		return err
	}
	return nil
}

func resolveOlcrtcPath() (string, error) {
	if path := os.Getenv("OLCRTC_PATH"); path != "" {
		return path, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "olcrtc"), nil
}

func envDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func tlsFilesFromEnv() (string, string, bool, error) {
	cert := strings.TrimSpace(os.Getenv("OLCRTC_MANAGER_TLS_CERT"))
	key := strings.TrimSpace(os.Getenv("OLCRTC_MANAGER_TLS_KEY"))
	if cert == "" && key == "" {
		return "", "", false, nil
	}
	if cert == "" || key == "" {
		return "", "", false, errors.New("OLCRTC_MANAGER_TLS_CERT and OLCRTC_MANAGER_TLS_KEY must be set together")
	}
	return cert, key, true, nil
}

func defaultString(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func normalizeSubscriptionPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, "/")
	if path == "" {
		return "sub", nil
	}
	if strings.Contains(path, "\\") || strings.Contains(path, "?") || strings.Contains(path, "#") {
		return "", errors.New("must be a plain URL path without query or fragment")
	}
	parts := strings.Split(path, "/")
	reserved := map[string]struct{}{
		"-":      {},
		"admin":  {},
		"api":    {},
		"assets": {},
	}
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", errors.New("must not contain empty, . or .. segments")
		}
		if strings.ContainsAny(part, " \t\r\n") {
			return "", errors.New("must not contain whitespace")
		}
		if i == 0 {
			if _, ok := reserved[part]; ok {
				return "", fmt.Errorf("must not start with reserved segment %q", part)
			}
		}
	}
	return strings.Join(parts, "/"), nil
}

func validateRefresh(refresh string) error {
	if refresh == "" {
		return nil
	}
	if len(refresh) < 2 {
		return errors.New("must use intervals like 5s, 10m, 6h or 1d")
	}
	unit := refresh[len(refresh)-1]
	if unit != 's' && unit != 'm' && unit != 'h' && unit != 'd' {
		return errors.New("must end with s, m, h or d")
	}
	value := refresh[:len(refresh)-1]
	if strings.HasPrefix(value, "0") {
		return errors.New("must be greater than zero")
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return errors.New("must use intervals like 5s, 10m, 6h or 1d")
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return errors.New("must be greater than zero")
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func subscriptionHandler(supervisor *Supervisor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := clientIDFromSubscriptionPath(r.URL.Path, supervisor.SubscriptionPath())
		if !ok {
			http.NotFound(w, r)
			return
		}

		sub, ok := supervisor.SubscriptionForClient(clientID, time.Now())
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(sub))
	})
}

func adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass := adminCredentials(configPathFromRequest(r))
		if user == "" || pass == "" {
			writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"setup_required": true})
			return
		}
		if cookie, err := r.Cookie("olcrtc_session"); err == nil && adminSessions.Valid(cookie.Value) {
			next.ServeHTTP(w, r)
			return
		}
		remote := remoteHost(r)
		if authLimiter.Blocked(remote) {
			http.Error(w, "too many auth failures", http.StatusTooManyRequests)
			return
		}
		gotUser, gotPass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			authLimiter.Fail(remote)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		authLimiter.Reset(remote)
		next.ServeHTTP(w, r)
	})
}

func configPathFromRequest(r *http.Request) string {
	if value, ok := r.Context().Value(configPathContextKey{}).(string); ok {
		return value
	}
	return adminConfigPath
}

type configPathContextKey struct{}

func authMeHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		user, pass := adminCredentials(configPath)
		if user == "" || pass == "" {
			writeJSON(w, map[string]any{"authenticated": false, "setup_required": true})
			return
		}
		if cookie, err := r.Cookie("olcrtc_session"); err == nil && adminSessions.Valid(cookie.Value) {
			writeJSON(w, map[string]any{"authenticated": true, "user": user})
			return
		}
		writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"authenticated": false})
	}
}

func loginHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user, pass := adminCredentials(configPath)
		if user == "" || pass == "" {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"setup_required": true})
			return
		}
		remote := remoteHost(r)
		if authLimiter.Blocked(remote) {
			http.Error(w, "too many auth failures", http.StatusTooManyRequests)
			return
		}
		userOK := subtle.ConstantTimeCompare([]byte(req.User), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(pass)) == 1
		if user == "" || pass == "" || !userOK || !passOK {
			authLimiter.Fail(remote)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		authLimiter.Reset(remote)
		token, err := adminSessions.Create()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, token)
		writeJSON(w, map[string]any{"authenticated": true, "user": user})
	}
}

func setupHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if user, pass := adminCredentials(configPath); user != "" && pass != "" {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"setup_required": false})
			return
		}
		var req struct {
			User     string `json:"user"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		req.User = strings.TrimSpace(req.User)
		if req.User == "" {
			req.User = "admin"
		}
		if len(req.Password) < 8 {
			http.Error(w, "password must contain at least 8 characters", http.StatusBadRequest)
			return
		}
		if err := updatePanelEnvPassword(configPath, req.User, req.Password); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		token, err := adminSessions.Create()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, token)
		writeJSON(w, map[string]any{"authenticated": true, "user": req.User})
	}
}

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "olcrtc_session",
		Value:    token,
		Path:     "/",
		MaxAge:   int((12 * time.Hour).Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie("olcrtc_session"); err == nil {
		adminSessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "olcrtc_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func changePasswordHandler(configPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			CurrentPassword string `json:"current_password"`
			NewPassword     string `json:"new_password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user, pass := adminCredentials(configPath)
		if subtle.ConstantTimeCompare([]byte(req.CurrentPassword), []byte(pass)) != 1 {
			http.Error(w, "current password is invalid", http.StatusUnauthorized)
			return
		}
		if len(req.NewPassword) < 8 {
			http.Error(w, "new password must contain at least 8 characters", http.StatusBadRequest)
			return
		}
		if err := updatePanelEnvPassword(configPath, user, req.NewPassword); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		adminSessions.Clear()
		writeJSON(w, map[string]any{"changed": true})
	}
}

func adminCredentials(configPath string) (string, string) {
	user := os.Getenv("OLCRTC_MANAGER_USER")
	pass := os.Getenv("OLCRTC_MANAGER_PASS")
	envPath := panelEnvPath(configPath)
	if values, err := readEnvFile(envPath); err == nil {
		user = defaultString(values["OLCRTC_MANAGER_USER"], user)
		pass = defaultString(values["OLCRTC_MANAGER_PASS"], pass)
	}
	return user, pass
}

func currentAdminUser(configPath string) string {
	user, _ := adminCredentials(configPath)
	return user
}

func panelEnvPath(configPath string) string {
	if path := os.Getenv("OLCRTC_MANAGER_ENV_FILE"); path != "" {
		return path
	}
	if configPath != "" {
		return filepath.Join(filepath.Dir(configPath), "panel.env")
	}
	return "/etc/olcrtc-manager/panel.env"
}

func readEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		values[strings.TrimSpace(parts[0])] = strings.Trim(strings.TrimSpace(parts[1]), `"'`)
	}
	return values, nil
}

func updatePanelEnvPassword(configPath, user, pass string) error {
	path := panelEnvPath(configPath)
	values, _ := readEnvFile(path)
	if values == nil {
		values = make(map[string]string)
	}
	values["OLCRTC_MANAGER_USER"] = defaultString(user, "admin")
	values["OLCRTC_MANAGER_PASS"] = pass
	data := fmt.Sprintf("OLCRTC_MANAGER_USER=%s\nOLCRTC_MANAGER_PASS=%s\n", shellQuote(values["OLCRTC_MANAGER_USER"]), shellQuote(values["OLCRTC_MANAGER_PASS"]))
	return os.WriteFile(path, []byte(data), 0o600)
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type authLimiterState struct {
	count int
	until time.Time
}

type authLimiterType struct {
	mu    sync.Mutex
	state map[string]authLimiterState
}

func newAuthLimiter() *authLimiterType {
	return &authLimiterType{state: make(map[string]authLimiterState)}
}

func (l *authLimiterType) Blocked(remote string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.state[remote]
	if time.Now().Before(state.until) {
		return true
	}
	return false
}

func (l *authLimiterType) Fail(remote string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.state[remote]
	state.count++
	if state.count >= 5 {
		state.until = time.Now().Add(time.Minute)
	}
	l.state[remote] = state
}

func (l *authLimiterType) Reset(remote string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.state, remote)
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]time.Time)}
}

func (s *sessionStore) Create() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = time.Now().Add(12 * time.Hour)
	return token, nil
}

func (s *sessionStore) Valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func (s *sessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func (s *sessionStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]time.Time)
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func adminFileServer() (http.Handler, error) {
	dist, err := fs.Sub(adminAssets, "web/dist")
	if err != nil {
		return nil, fmt.Errorf("load admin assets: %w", err)
	}
	return http.FileServer(http.FS(dist)), nil
}

func adminPageHandler(files http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https://api.qrserver.com; style-src 'self' 'unsafe-inline'; script-src 'self'")
		next.ServeHTTP(w, r)
	})
}

func clientIDFromPath(path string) (string, bool) {
	clientID := strings.Trim(path, "/")
	if clientID == "" || strings.Contains(clientID, "/") {
		return "", false
	}
	return clientID, true
}

func clientIDFromSubscriptionPath(path, subscriptionPath string) (string, bool) {
	subscriptionPath, err := normalizeSubscriptionPath(subscriptionPath)
	if err != nil {
		return "", false
	}
	if subscriptionPath == "" {
		return clientIDFromPath(path)
	}
	prefix := "/" + subscriptionPath + "/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	return clientIDFromPath(strings.TrimPrefix(path, "/"+subscriptionPath))
}

func subscription(cfg Config, now time.Time) string {
	return subscriptionForLocations(cfg.Name, cfg.Refresh, cfg.Locations, Quota{}, now)
}

func subscriptionForClient(cfg Config, clientID string, now time.Time) (string, bool) {
	for _, client := range cfg.Clients {
		if client.ClientID == clientID {
			refresh := effectiveRefresh(cfg.Refresh, client.Refresh)
			if len(client.Locations) == 0 {
				return "", false
			}
			if quotaStatus(client.Quota, now) != "active" {
				return subscriptionForLocations(cfg.Name, refresh, nil, client.Quota, now), true
			}
			return subscriptionForLocations(cfg.Name, refresh, client.Locations, client.Quota, now), true
		}
	}
	locations := make([]Location, 0)
	for _, loc := range cfg.Locations {
		if loc.ClientID == clientID {
			locations = append(locations, loc)
		}
	}
	if len(locations) == 0 {
		return "", false
	}
	return subscriptionForLocations(cfg.Name, cfg.Refresh, locations, Quota{}, now), true
}

func effectiveRefresh(globalRefresh, clientRefresh string) string {
	if strings.TrimSpace(clientRefresh) != "" {
		return strings.TrimSpace(clientRefresh)
	}
	return strings.TrimSpace(globalRefresh)
}

func subscriptionForLocations(name, refresh string, locations []Location, quota Quota, now time.Time) string {
	var b bytes.Buffer
	if name != "" {
		fmt.Fprintf(&b, "#name: %s\n", name)
	}
	fmt.Fprintf(&b, "#update: %d\n", now.Unix())
	if refresh != "" {
		fmt.Fprintf(&b, "#refresh: %s\n", refresh)
	}
	fmt.Fprintln(&b)
	if quota.SpeedMbps > 0 {
		fmt.Fprintf(&b, "#quota-speed-mbps: %d\n", quota.SpeedMbps)
	}
	if quota.TrafficGB > 0 {
		fmt.Fprintf(&b, "#quota-traffic-gb: %d\n", quota.TrafficGB)
		fmt.Fprintf(&b, "#quota-used-gb: %d\n", quota.UsedGB)
		fmt.Fprintf(&b, "#quota-used-bytes: %d\n", quotaUsedBytes(quota))
	}
	if quota.ExpiresAt != "" {
		fmt.Fprintf(&b, "#quota-expires-at: %s\n", quota.ExpiresAt)
	}
	if quota.SpeedMbps > 0 || quota.TrafficGB > 0 || quota.ExpiresAt != "" {
		fmt.Fprintf(&b, "#quota-status: %s\n\n", quotaStatus(quota, now))
	}

	for _, loc := range locations {
		fmt.Fprintln(&b, locationURI(loc))
		if loc.Name != "" {
			fmt.Fprintf(&b, "##name: %s\n", loc.Name)
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}

func locationURI(loc Location) string {
	payload := payloadString(loc.Transport.Payload)
	return fmt.Sprintf("olcrtc://%s?%s%s@%s#%s$%s",
		loc.Carrier,
		loc.Transport.Type,
		payload,
		loc.Endpoint.RoomID,
		loc.Endpoint.Key,
		loc.Name,
	)
}

func payloadString(payload map[string]string) string {
	if len(payload) == 0 {
		return ""
	}

	parts := make([]string, 0, len(payload))
	for _, key := range sortedKeys(payload) {
		parts = append(parts, key+"="+payload[key])
	}
	return "<" + strings.Join(parts, "&") + ">"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
