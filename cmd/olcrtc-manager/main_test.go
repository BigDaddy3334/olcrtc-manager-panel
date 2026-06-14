package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionUsesDocumentedPayloadKeys(t *testing.T) {
	cfg := Config{
		Name: "ScumVPN",
		Port: 8888,
		Locations: []Location{
			{
				Name:     "Netherlands",
				ClientID: "user",
				Endpoint: Endpoint{RoomID: "room-01", Key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				Carrier:  "wbstream",
				Transport: Transport{
					Type: "vp8channel",
					Payload: map[string]string{
						"vp8-fps":   "60",
						"vp8-batch": "64",
					},
				},
				Link: "direct",
				Data: "data",
				DNS:  "1.1.1.1:53",
			},
		},
	}

	got := subscription(cfg, time.Unix(1778011200, 0))

	want := "olcrtc://wbstream?vp8channel<vp8-batch=64&vp8-fps=60>@room-01#aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa$Netherlands"
	if !strings.Contains(got, want) {
		t.Fatalf("subscription missing URI\nwant: %s\ngot:\n%s", want, got)
	}
	if !strings.Contains(got, "#name: ScumVPN\n#update: 1778011200") {
		t.Fatalf("subscription missing global metadata:\n%s", got)
	}
	if !strings.Contains(got, "##name: Netherlands") {
		t.Fatalf("subscription missing location metadata:\n%s", got)
	}
}

func TestServerConfigMapsPayloadToYAMLSections(t *testing.T) {
	loc := Location{
		ClientID: "user",
		Endpoint: Endpoint{RoomID: "room-01", Key: "key"},
		Carrier:  "wbstream",
		Transport: Transport{
			Type: "seichannel",
			Payload: map[string]string{
				"fps":    "60",
				"batch":  "64",
				"frag":   "900",
				"ack-ms": "2000",
			},
		},
		Link: "direct",
		Data: "data",
		DNS:  "1.1.1.1:53",
	}

	got, err := serverConfig(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "srv" || got.Auth.Provider != "wbstream" || got.Room.ID != "room-01" || got.Crypto.Key != "key" {
		t.Fatalf("server config core fields = %#v", got)
	}
	if got.Net.Transport != "seichannel" || got.Net.DNS != "1.1.1.1:53" || got.Data != "data" {
		t.Fatalf("server config net/data fields = %#v", got)
	}
	if got.SEI == nil {
		t.Fatal("server config missing sei section")
	}
	if got.SEI.FPS != 60 || got.SEI.BatchSize != 64 || got.SEI.FragmentSize != 900 || got.SEI.AckTimeoutMS != 2000 {
		t.Fatalf("sei config = %#v", got.SEI)
	}
}

func TestServerConfigMapsUpstreamProxyAuth(t *testing.T) {
	loc := testLocation("room-01", "Proxy")
	loc.Proxy = Socks5Proxy{
		Addr: "127.0.0.1",
		Port: 1080,
		User: "proxy-user",
		Pass: "proxy-pass",
	}

	got, err := serverConfig(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got.SOCKS.ProxyAddr != "127.0.0.1" || got.SOCKS.ProxyPort != 1080 ||
		got.SOCKS.ProxyUser != "proxy-user" || got.SOCKS.ProxyPass != "proxy-pass" {
		t.Fatalf("socks proxy config = %#v", got.SOCKS)
	}
}

func TestServerConfigAllowsVideoAutoQRSize(t *testing.T) {
	loc := testLocation("room-01", "Video")
	loc.Transport = Transport{
		Type: "videochannel",
		Payload: map[string]string{
			"video-w":       "1080",
			"video-h":       "1080",
			"video-fps":     "60",
			"video-bitrate": "5000k",
			"video-hw":      "none",
			"video-codec":   "qrcode",
			"video-qr-size": "0",
			"video-tile-rs": "0",
		},
	}

	got, err := serverConfig(loc)
	if err != nil {
		t.Fatal(err)
	}
	if got.Video == nil {
		t.Fatal("server config missing video section")
	}
	if got.Video.QRSize != 0 || got.Video.TileRS != 0 {
		t.Fatalf("video zero-valued options = %#v", got.Video)
	}
}

func TestSubscriptionForClientIncludesOnlyClientLocations(t *testing.T) {
	userLoc := testLocation("room-01", "Netherlands")
	otherLoc := testLocation("room-02", "Germany")
	otherLoc.ClientID = "other"
	cfg := testConfig(userLoc, otherLoc)

	got, ok := subscriptionForClient(cfg, "user", time.Unix(1778011200, 0))
	if !ok {
		t.Fatal("subscriptionForClient returned ok=false")
	}
	if !strings.Contains(got, "$Netherlands") {
		t.Fatalf("subscription missing user location:\n%s", got)
	}
	if strings.Contains(got, "$Germany") {
		t.Fatalf("subscription included another client's location:\n%s", got)
	}
}

func TestSubscriptionForClientIncludesQuotaMetadata(t *testing.T) {
	loc := testLocation("room-01", "Netherlands")
	cfg := Config{
		Name: "ScumVPN",
		Port: 8888,
		Clients: []Client{
			{
				ClientID: "user",
				Quota: Quota{
					SpeedMbps: 50,
					TrafficGB: 100,
					UsedGB:    25,
					ExpiresAt: "2026-06-01",
				},
				Locations: []Location{loc},
			},
		},
		Locations: []Location{loc},
	}

	got, ok := subscriptionForClient(cfg, "user", time.Unix(1778011200, 0))
	if !ok {
		t.Fatal("subscriptionForClient returned ok=false")
	}
	for _, want := range []string{
		"#quota-speed-mbps: 50",
		"#quota-traffic-gb: 100",
		"#quota-used-gb: 25",
		"#quota-expires-at: 2026-06-01",
		"#quota-status: active",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("subscription missing %q:\n%s", want, got)
		}
	}
}

func TestSubscriptionIncludesGlobalRefresh(t *testing.T) {
	cfg := testConfig(testLocation("room-01", "Netherlands"))
	cfg.Refresh = "10m"

	got := subscription(cfg, time.Unix(1778011200, 0))

	if !strings.Contains(got, "#refresh: 10m") {
		t.Fatalf("subscription missing global refresh:\n%s", got)
	}
}

func TestSubscriptionForClientRefreshOverridesGlobal(t *testing.T) {
	loc := testLocation("room-01", "Netherlands")
	cfg := Config{
		Name:    "ScumVPN",
		Port:    8888,
		Refresh: "10m",
		Clients: []Client{
			{
				ClientID:  "user",
				Refresh:   "1m",
				Locations: []Location{loc},
			},
		},
		Locations: []Location{loc},
	}

	got, ok := subscriptionForClient(cfg, "user", time.Unix(1778011200, 0))
	if !ok {
		t.Fatal("subscriptionForClient returned ok=false")
	}
	if !strings.Contains(got, "#refresh: 1m") {
		t.Fatalf("subscription missing client refresh:\n%s", got)
	}
	if strings.Contains(got, "#refresh: 10m") {
		t.Fatalf("subscription used global refresh despite client override:\n%s", got)
	}
}

func TestSubscriptionForClientRejectsUnknownClient(t *testing.T) {
	cfg := testConfig(testLocation("room-01", "Netherlands"))

	if got, ok := subscriptionForClient(cfg, "missing", time.Unix(1778011200, 0)); ok || got != "" {
		t.Fatalf("subscriptionForClient = %q, %v; want empty, false", got, ok)
	}
}

func TestSubscriptionHandlerServesDefaultSubPath(t *testing.T) {
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (*process, error) {
		return &process{location: loc, logs: newLogBuffer(1), running: true}, nil
	})
	loc := testLocation("room-01", "Netherlands")
	if err := supervisor.StartAll(context.Background(), testConfig(loc)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sub/user/", nil)
	rec := httptest.NewRecorder()
	subscriptionHandler(supervisor).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); !strings.Contains(got, "#key$Netherlands") {
		t.Fatalf("response missing user subscription:\n%s", got)
	}
}

func TestSubscriptionHandlerServesConfiguredBasePath(t *testing.T) {
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (*process, error) {
		return &process{location: loc, logs: newLogBuffer(1), running: true}, nil
	})
	loc := testLocation("room-01", "Netherlands")
	cfg := testConfig(loc)
	cfg.SubscriptionPath = "sub"
	if err := supervisor.StartAll(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sub/user/", nil)
	rec := httptest.NewRecorder()
	subscriptionHandler(supervisor).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); !strings.Contains(got, "#key$Netherlands") {
		t.Fatalf("response missing user subscription:\n%s", got)
	}

	rec = httptest.NewRecorder()
	subscriptionHandler(supervisor).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/user/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("root status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSubscriptionHandlerRejectsRootAndUnknownClient(t *testing.T) {
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (*process, error) {
		return &process{location: loc, logs: newLogBuffer(1), running: true}, nil
	})
	if err := supervisor.StartAll(context.Background(), testConfig(testLocation("room-01", "Netherlands"))); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/missing", "/user/", "/sub/user/extra"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		subscriptionHandler(supervisor).ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}
}

func TestUpdateSettingsNormalizesSubscriptionPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := writeConfig(configPath, Config{
		Name: "Old",
		Port: 8888,
		Clients: []Client{{
			ClientID:  "user",
			Locations: []Location{testLocation("room-01", "Netherlands")},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	cfg, restartRequired, err := updateSettings(configPath, updateSettingsRequest{
		Name:             "New",
		Port:             9443,
		SubscriptionPath: "/subscription/",
		Refresh:          "10m",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restartRequired {
		t.Fatal("restartRequired = false, want true")
	}
	if cfg.Name != "New" || cfg.Port != 9443 || cfg.SubscriptionPath != "subscription" || cfg.Refresh != "10m" {
		t.Fatalf("settings = %#v", cfg)
	}
}

func TestConfigDefaultsSubscriptionPathToSub(t *testing.T) {
	cfg := Config{Name: "ScumVPN", Port: 8888}
	cfg.Normalize()
	if cfg.SubscriptionPath != "sub" {
		t.Fatalf("SubscriptionPath = %q, want sub", cfg.SubscriptionPath)
	}
}

func TestConfigValidatesRefreshIntervals(t *testing.T) {
	for _, refresh := range []string{"5s", "10m", "6h", "1d"} {
		cfg := Config{Name: "ScumVPN", Port: 8888, Refresh: refresh}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate(%q) = %v, want nil", refresh, err)
		}
	}

	cfg := Config{Name: "ScumVPN", Port: 8888, Refresh: "10w"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for invalid refresh")
	}
}

func TestParseProcStatusMemoryBytes(t *testing.T) {
	data := []byte("Name:\tolcrtc\nVmPeak:\t  204800 kB\nVmRSS:\t   12345 kB\n")

	if got := parseProcStatusMemoryBytes(data); got != 12345*1024 {
		t.Fatalf("memory bytes = %d, want %d", got, 12345*1024)
	}
}

func TestLogRequestTargetAllowsSlashesInRoomID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/logs/?client_id=user&room_id=https%3A%2F%2Fmeet.example.org%2Froom&transport=datachannel", nil)
	clientID, roomID, transport := logRequestTarget(req)
	if clientID != "user" || roomID != "https://meet.example.org/room" || transport != "datachannel" {
		t.Fatalf("log target = %q, %q, %q", clientID, roomID, transport)
	}
}

func TestTLSFilesFromEnv(t *testing.T) {
	t.Setenv("OLCRTC_MANAGER_TLS_CERT", "/tmp/panel.crt")
	t.Setenv("OLCRTC_MANAGER_TLS_KEY", "/tmp/panel.key")

	cert, key, enabled, err := tlsFilesFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || cert != "/tmp/panel.crt" || key != "/tmp/panel.key" {
		t.Fatalf("tls files = %q, %q, %v", cert, key, enabled)
	}
}

func TestTLSFilesFromEnvRequiresPair(t *testing.T) {
	t.Setenv("OLCRTC_MANAGER_TLS_CERT", "/tmp/panel.crt")
	t.Setenv("OLCRTC_MANAGER_TLS_KEY", "")

	if _, _, _, err := tlsFilesFromEnv(); err == nil {
		t.Fatal("expected error for partial TLS env")
	}
}

func TestDeleteLastClient(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := writeConfig(configPath, Config{
		Name: "ScumVPN",
		Port: 8888,
		Clients: []Client{{
			ClientID:  "user",
			Locations: []Location{testLocation("room-01", "Netherlands")},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := deleteClient(configPath, "user"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients) != 0 || len(cfg.Locations) != 0 {
		t.Fatalf("config after delete = %#v", cfg)
	}
}

func TestFirstRunSetupCreatesAdminSession(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	t.Setenv("OLCRTC_MANAGER_ENV_FILE", filepath.Join(dir, "panel.env"))
	t.Setenv("OLCRTC_MANAGER_USER", "")
	t.Setenv("OLCRTC_MANAGER_PASS", "")
	adminSessions.Clear()

	rec := httptest.NewRecorder()
	authMeHandler(configPath).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/me", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("auth me status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"setup_required": true`) {
		t.Fatalf("auth me did not request setup: %s", rec.Body.String())
	}

	body := bytes.NewBufferString(`{"user":"admin","password":"firstpass123"}`)
	rec = httptest.NewRecorder()
	setupHandler(configPath).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/auth/setup", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	for _, cookie := range rec.Result().Cookies() {
		req.AddCookie(cookie)
	}
	rec = httptest.NewRecorder()
	authMeHandler(configPath).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"authenticated": true`) {
		t.Fatalf("auth me after setup status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	adminAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("protected status without session = %d, want 401", rec.Code)
	}
}

func TestConfigRejectsAnyRoomID(t *testing.T) {
	cfg := Config{
		Name: "ScumVPN",
		Port: 8888,
		Locations: []Location{
			{
				ClientID:  "user",
				Endpoint:  Endpoint{RoomID: "any", Key: "key"},
				Carrier:   "wbstream",
				Transport: Transport{Type: "datachannel"},
				Link:      "direct",
				Data:      "data",
				DNS:       "1.1.1.1:53",
			},
		},
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for room_id=any")
	}
}

func TestConfigRejectsUnsupportedCarrier(t *testing.T) {
	loc := testLocation("room-01", "Unsupported")
	loc.Carrier = "jazz"
	cfg := testConfig(loc)

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for unsupported carrier")
	}
}

func TestParsePeerSummaryLine(t *testing.T) {
	count, devices, ok := parsePeerSummaryLine("2026/06/14 12:00:00 Current peers count: 2, Devices: [dev-b, dev-a]")
	if !ok {
		t.Fatal("parsePeerSummaryLine ok=false")
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}
	if strings.Join(devices, ",") != "dev-b,dev-a" {
		t.Fatalf("devices = %#v", devices)
	}

	count, devices, ok = parsePeerSummaryLine("Current peers count: 0, Devices: []")
	if !ok || count != 0 || len(devices) != 0 {
		t.Fatalf("empty summary = count %d devices %#v ok %v", count, devices, ok)
	}
}

func TestConfigRejectsIncompleteProxy(t *testing.T) {
	loc := testLocation("room-01", "Proxy")
	loc.Proxy = Socks5Proxy{Port: 1080}
	cfg := testConfig(loc)

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for proxy without address")
	}
}

func TestConfigAllowsFreshInstallWithoutLocations(t *testing.T) {
	cfg := Config{
		Name:    "OlcRTC VPS",
		Port:    8888,
		Clients: []Client{},
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestGenerateRoomIDUsesLocalJitsiUUID(t *testing.T) {
	roomID, err := generateRoomID(context.Background(), "/bin/false", "jitsi", "1.1.1.1:53")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(roomID, defaultGeneratedJitsiBase+"/") {
		t.Fatalf("roomID = %q, want default jitsi base", roomID)
	}
}

func TestGenerateRoomIDRejectsProvidersWithoutRoomCreation(t *testing.T) {
	if _, err := generateRoomID(context.Background(), "/bin/false", "wbstream", "1.1.1.1:53"); err == nil {
		t.Fatal("generateRoomID(wbstream) error = nil")
	}
}

func TestRegenerateClientRoomPreservesJitsiBase(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	loc := testLocation("https://meet.cryptopro.ru/old-room", "Jitsi")
	loc.Carrier = "jitsi"
	loc.Transport = Transport{Type: "datachannel"}
	if err := writeConfig(configPath, Config{
		Name:    "ScumVPN",
		Port:    8888,
		Clients: []Client{{ClientID: "user", Locations: []Location{loc}}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := regenerateClientRoom(context.Background(), configPath, "/bin/false", "user"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	roomID := cfg.Clients[0].Locations[0].Endpoint.RoomID
	if !strings.HasPrefix(roomID, "https://meet.cryptopro.ru/") || strings.HasSuffix(roomID, "/old-room") {
		t.Fatalf("regenerated roomID = %q", roomID)
	}
}

func TestTransportUnmarshalPayload(t *testing.T) {
	var cfg Config
	data := []byte(`{
		"version": 4,
		"name": "ScumVPN",
		"port": 8888,
		"clients": [{
			"client-id": "user",
			"locations": [{
				"name": "Netherlands",
				"endpoint": {"room_id": "room-01", "key": "key"},
				"carrier": "wbstream",
				"transport": {
					"type": "vp8channel",
					"payload": {
						"vp8-fps": 60,
						"vp8-batch": 64
					}
				},
				"link": "direct",
				"data": "data",
				"dns": "1.1.1.1:53"
			}]
		}]
	}`)

	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Locations[0].Transport.Payload["vp8-fps"]; got != "60" {
		t.Fatalf("vp8-fps = %q, want 60", got)
	}
}

func TestConfigUnmarshalClientsFormat(t *testing.T) {
	var cfg Config
	data := []byte(`{
		"vesion": 1,
		"name": "ScumVPN",
		"port": 8888,
		"clients": [{
			"client-id": "mark",
			"locations": [
				{
					"name": "Netherlands",
					"carrier": "jitsi",
					"transport": {"type": "datachannel"},
					"link": "direct",
					"data": "data",
					"dns": "1.1.1.1:53",
					"endpoint": {"room_id": "room-01", "key": "key"}
				},
				{
					"name": "Netherlands VP8",
					"carrier": "wbstream",
					"transport": {
						"type": "vp8channel",
						"payload": {
							"vp8-fps": 60,
							"vp8-batch": 64
						}
					},
					"link": "direct",
					"data": "data",
					"dns": "1.1.1.1:53",
					"endpoint": {"room_id": "room-02", "key": "key"}
				}
			]
		}]
	}`)

	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 {
		t.Fatalf("Version = %d, want 1", cfg.Version)
	}
	if len(cfg.Locations) != 2 {
		t.Fatalf("locations = %d, want 2", len(cfg.Locations))
	}
	if got := cfg.Locations[0].ClientID; got != "mark" {
		t.Fatalf("client-id = %q, want mark", got)
	}
	if got := cfg.Locations[1].Transport.Payload["vp8-fps"]; got != "60" {
		t.Fatalf("vp8-fps = %q, want 60", got)
	}
}

func TestAddClientUsesExplicitLocationsWithoutRoomGeneration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	existing := testLocation("room-01", "Default")
	if err := writeConfig(configPath, Config{
		Name: "ScumVPN",
		Port: 8888,
		Clients: []Client{{
			ClientID:  "default",
			Locations: []Location{existing},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{
		"client_id": "alice",
		"locations": [
			{
				"name": "WB",
				"room_id": "wb-room",
				"key": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"carrier": "wbstream",
				"transport": "vp8channel",
				"dns": "1.1.1.1:53"
			},
			{
				"name": "Telemost",
				"room_id": "tele-room",
				"key": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				"carrier": "telemost",
				"transport": "vp8channel",
				"payload": {"vp8-fps": "60", "vp8-batch": "64"},
				"dns": "1.1.1.1:53"
			}
		]
	}`)

	if _, err := addClientFromRequest(context.Background(), configPath, "/bin/false", httptest.NewRequest(http.MethodPost, "/api/clients", body)); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var alice *Client
	for i := range cfg.Clients {
		if cfg.Clients[i].ClientID == "alice" {
			alice = &cfg.Clients[i]
		}
	}
	if alice == nil {
		t.Fatal("alice client was not saved")
	}
	if len(alice.Locations) != 2 {
		t.Fatalf("locations = %d, want 2", len(alice.Locations))
	}
	if got := alice.Locations[0].Endpoint.RoomID; got != "wb-room" {
		t.Fatalf("first room_id = %q, want wb-room", got)
	}
	if got := alice.Locations[1].Carrier + "/" + alice.Locations[1].Transport.Type; got != "telemost/vp8channel" {
		t.Fatalf("second carrier/transport = %q", got)
	}
}

func TestUpdateClientReplacesLocations(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	loc1 := testLocation("room-01", "WB")
	loc2 := testLocation("room-02", "Telemost")
	loc2.Carrier = "telemost"
	loc2.Transport = Transport{Type: "vp8channel", Payload: map[string]string{"vp8-fps": "60", "vp8-batch": "64"}}
	if err := writeConfig(configPath, Config{
		Name: "ScumVPN",
		Port: 8888,
		Clients: []Client{{
			ClientID:  "user",
			Locations: []Location{loc1, loc2},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{
		"locations": [{
			"name": "Only WB",
			"room_id": "room-01",
			"key": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"carrier": "wbstream",
			"transport": "vp8channel",
			"dns": "1.1.1.1:53"
		}]
	}`)
	if err := updateClientFromRequest(context.Background(), configPath, "/bin/false", "user", httptest.NewRequest(http.MethodPut, "/api/clients/user", body)); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients[0].Locations) != 1 {
		t.Fatalf("locations = %d, want 1", len(cfg.Clients[0].Locations))
	}
	if got := cfg.Clients[0].Locations[0].Name; got != "Only WB" {
		t.Fatalf("location name = %q, want Only WB", got)
	}
}

func TestDeleteLastLocation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := writeConfig(configPath, Config{
		Name: "ScumVPN",
		Port: 8888,
		Clients: []Client{{
			ClientID:  "user",
			Locations: []Location{testLocation("room-01", "WB")},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := deleteLocation(configPath, "user", "room-01"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients) != 1 {
		t.Fatalf("clients = %d, want 1", len(cfg.Clients))
	}
	if len(cfg.Clients[0].Locations) != 0 || len(cfg.Locations) != 0 {
		t.Fatalf("config after deleting last location = %#v", cfg)
	}
}

func TestUpdateClientQuotaKeepsLocations(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	loc1 := testLocation("room-01", "WB")
	loc2 := testLocation("room-02", "Telemost")
	if err := writeConfig(configPath, Config{
		Name: "ScumVPN",
		Port: 8888,
		Clients: []Client{{
			ClientID:  "user",
			Locations: []Location{loc1, loc2},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{"quota":{"speed_mbps":50,"traffic_gb":100}}`)
	if err := updateClientFromRequest(context.Background(), configPath, "/bin/false", "user", httptest.NewRequest(http.MethodPut, "/api/clients/user", body)); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Clients[0].Locations); got != 2 {
		t.Fatalf("locations = %d, want 2", got)
	}
	if got := cfg.Clients[0].Quota.SpeedMbps; got != 50 {
		t.Fatalf("speed_mbps = %d, want 50", got)
	}
}

func TestUpdateClientRenamesLocationOwners(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	loc := testLocation("room-01", "WB")
	if err := writeConfig(configPath, Config{
		Name: "ScumVPN",
		Port: 8888,
		Clients: []Client{{
			ClientID:  "user",
			Locations: []Location{loc},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	body := bytes.NewBufferString(`{"client_id":"renamed"}`)
	if err := updateClientFromRequest(context.Background(), configPath, "/bin/false", "user", httptest.NewRequest(http.MethodPut, "/api/clients/user", body)); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Clients[0].ClientID; got != "renamed" {
		t.Fatalf("client_id = %q, want renamed", got)
	}
	if got := cfg.Clients[0].Locations[0].ClientID; got != "renamed" {
		t.Fatalf("location client-id = %q, want renamed", got)
	}
}

func TestSupervisorReloadStartsAddedLocationAndUpdatesSubscription(t *testing.T) {
	loc1 := testLocation("room-01", "Netherlands")
	loc2 := testLocation("room-02", "Germany")
	started := make([]string, 0)
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (*process, error) {
		started = append(started, locationKey(loc))
		return &process{location: loc, logs: newLogBuffer(1), running: true}, nil
	})

	if err := supervisor.StartAll(context.Background(), testConfig(loc1)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reload(context.Background(), testConfig(loc1, loc2)); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(started, ","); got != "user:room-01:vp8channel,user:room-02:vp8channel" {
		t.Fatalf("started = %q, want user:room-01:vp8channel,user:room-02:vp8channel", got)
	}
	if got := supervisor.Subscription(time.Unix(1778011200, 0)); !strings.Contains(got, "$Germany") {
		t.Fatalf("subscription was not updated:\n%s", got)
	}
}

func TestSupervisorReloadRestartsChangedLocation(t *testing.T) {
	loc := testLocation("room-01", "Netherlands")
	changed := loc
	changed.Endpoint.RoomID = "room-02"
	started := make([]string, 0)
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (*process, error) {
		started = append(started, loc.Endpoint.RoomID)
		return &process{location: loc, logs: newLogBuffer(1), running: true}, nil
	})

	if err := supervisor.StartAll(context.Background(), testConfig(loc)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reload(context.Background(), testConfig(changed)); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(started, ","); got != "room-01,room-02" {
		t.Fatalf("started room ids = %q, want room-01,room-02", got)
	}
	if got := supervisor.Subscription(time.Unix(1778011200, 0)); !strings.Contains(got, "@room-02#") {
		t.Fatalf("subscription did not use changed location:\n%s", got)
	}
}

func TestSupervisorReloadFailureKeepsCurrentConfig(t *testing.T) {
	loc1 := testLocation("room-01", "Netherlands")
	loc2 := testLocation("room-02", "Germany")
	startErr := errors.New("boom")
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (*process, error) {
		if loc.Endpoint.RoomID == "room-02" {
			return nil, startErr
		}
		return &process{location: loc, logs: newLogBuffer(1), running: true}, nil
	})

	if err := supervisor.StartAll(context.Background(), testConfig(loc1)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reload(context.Background(), testConfig(loc1, loc2)); !errors.Is(err, startErr) {
		t.Fatalf("Reload error = %v, want %v", err, startErr)
	}

	if got := supervisor.Subscription(time.Unix(1778011200, 0)); strings.Contains(got, "$Germany") {
		t.Fatalf("failed reload changed subscription:\n%s", got)
	}
}

func testConfig(locations ...Location) Config {
	return Config{
		Name:      "ScumVPN",
		Port:      8888,
		Locations: locations,
	}
}

func testLocation(roomID, name string) Location {
	return Location{
		Name:      name,
		ClientID:  "user",
		Endpoint:  Endpoint{RoomID: roomID, Key: "key"},
		Carrier:   "wbstream",
		Transport: Transport{Type: "vp8channel"},
		Link:      "direct",
		Data:      "data",
		DNS:       "1.1.1.1:53",
	}
}
