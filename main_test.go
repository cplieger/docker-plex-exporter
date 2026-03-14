package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"pgregory.net/rapid"
)

func TestMain(m *testing.M) {
	retryBaseDelay = time.Microsecond
	os.Exit(m.Run())
}

// testMeta constructs a sessionMetadata from JSON to avoid anonymous struct
// tag mismatches in test literals. The JSON tags on Player/Session/User
// sub-structs make direct struct literal construction verbose and fragile.
func testMeta(t *testing.T, jsonStr string) sessionMetadata {
	t.Helper()
	var m sessionMetadata
	if err := json.Unmarshal([]byte(jsonStr), &m); err != nil {
		t.Fatalf("testMeta: %v", err)
	}
	return m
}

func TestTranscodeKind(t *testing.T) {
	tests := []struct {
		name string
		ts   wsTranscodeSession
		want string
	}{
		{
			name: "video only",
			ts:   wsTranscodeSession{VideoDecision: "transcode", AudioDecision: "copy"},
			want: "video",
		},
		{
			name: "audio only",
			ts:   wsTranscodeSession{AudioDecision: "transcode", VideoDecision: "copy"},
			want: "audio",
		},
		{
			name: "both",
			ts:   wsTranscodeSession{VideoDecision: "transcode", AudioDecision: "transcode"},
			want: "both",
		},
		{
			name: "none direct play",
			ts:   wsTranscodeSession{VideoDecision: "copy", AudioDecision: "copy"},
			want: valNone,
		},
		{
			name: "codec change implies video transcode",
			ts:   wsTranscodeSession{SourceVideoCodec: "hevc", VideoCodec: "h264"},
			want: "video",
		},
		{
			name: "codec change implies audio transcode",
			ts:   wsTranscodeSession{SourceAudioCodec: "truehd", AudioCodec: "aac"},
			want: "audio",
		},
		{
			name: "same codecs no transcode",
			ts:   wsTranscodeSession{SourceVideoCodec: "h264", VideoCodec: "h264", SourceAudioCodec: "aac", AudioCodec: "aac"},
			want: valNone,
		},
		{
			name: "whitespace trimmed",
			ts:   wsTranscodeSession{VideoDecision: " transcode ", AudioDecision: " copy "},
			want: "video",
		},
		{
			name: "empty session",
			ts:   wsTranscodeSession{},
			want: valNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := transcodeKind(&tt.ts)
			if got != tt.want {
				t.Errorf("transcodeKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubtitleAction(t *testing.T) {
	tests := []struct {
		name string
		ts   wsTranscodeSession
		want string
	}{
		{name: "burn", ts: wsTranscodeSession{SubtitleDecision: "burn"}, want: valBurn},
		{name: "burn-in", ts: wsTranscodeSession{SubtitleDecision: "burn-in"}, want: valBurn},
		{name: "copy", ts: wsTranscodeSession{SubtitleDecision: "copy"}, want: valCopy},
		{name: "copying", ts: wsTranscodeSession{SubtitleDecision: "copying"}, want: valCopy},
		{name: "transcode", ts: wsTranscodeSession{SubtitleDecision: "transcode"}, want: valTranscode},
		{name: "transcoding", ts: wsTranscodeSession{SubtitleDecision: "transcoding"}, want: valTranscode},
		{
			name: "empty with srt container implies copy",
			ts:   wsTranscodeSession{Container: "srt"},
			want: valCopy,
		},
		{
			name: "empty with video transcode implies burn",
			ts:   wsTranscodeSession{VideoDecision: "transcode"},
			want: valBurn,
		},
		{
			name: "empty no transcode",
			ts:   wsTranscodeSession{},
			want: valNone,
		},
		{
			name: "unknown value passed through",
			ts:   wsTranscodeSession{SubtitleDecision: "embed"},
			want: "embed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := subtitleAction(&tt.ts)
			if got != tt.want {
				t.Errorf("subtitleAction() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionLabels(t *testing.T) {
	tests := []struct {
		name                         string
		meta                         sessionMetadata
		wantTitle, wantChild, wantGC string
	}{
		{
			name:      "movie",
			meta:      sessionMetadata{Type: "movie", Title: "Inception"},
			wantTitle: "Inception",
		},
		{
			name: "episode",
			meta: sessionMetadata{
				Type: "episode", GrandparentTitle: "Breaking Bad",
				ParentTitle: "Season 1", Title: "Pilot",
			},
			wantTitle: "Breaking Bad", wantChild: "Season 1", wantGC: "Pilot",
		},
		{
			name: "track",
			meta: sessionMetadata{
				Type: "track", GrandparentTitle: "Pink Floyd",
				ParentTitle: "The Wall", Title: "Comfortably Numb",
			},
			wantTitle: "Pink Floyd", wantChild: "The Wall", wantGC: "Comfortably Numb",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			title, child, gc := sessionLabels(&tt.meta)
			if title != tt.wantTitle || child != tt.wantChild || gc != tt.wantGC {
				t.Errorf("sessionLabels() = (%q, %q, %q), want (%q, %q, %q)",
					title, child, gc, tt.wantTitle, tt.wantChild, tt.wantGC)
			}
		})
	}
}

func TestContentTypeLabel(t *testing.T) {
	tests := []struct {
		libType string
		want    string
	}{
		{libMovie, "movies"},
		{libShow, "episodes"},
		{libArtist, "tracks"},
		{"photo", "photos"},
		{"homevideo", "items"},
		{"other", "items"},
	}
	for _, tt := range tests {
		t.Run(tt.libType, func(t *testing.T) {
			if got := contentTypeLabel(tt.libType); got != tt.want {
				t.Errorf("contentTypeLabel(%q) = %q, want %q", tt.libType, got, tt.want)
			}
		})
	}
}

func TestIsLibraryType(t *testing.T) {
	valid := []string{"movie", "show", "artist", "photo", "homevideo"}
	for _, v := range valid {
		if !isLibraryType(v) {
			t.Errorf("isLibraryType(%q) = false, want true", v)
		}
	}
	invalid := []string{"", "clip", "playlist", "other"}
	for _, v := range invalid {
		if isLibraryType(v) {
			t.Errorf("isLibraryType(%q) = true, want false", v)
		}
	}
}

func TestMatchesTranscode(t *testing.T) {
	tests := []struct {
		name string
		sess session
		key  string
		want bool
	}{
		{
			name: "pending matches any key",
			sess: session{transcodeType: valPending},
			key:  "abc123",
			want: true,
		},
		{
			name: "part key contains transcode key",
			sess: session{
				meta: sessionMetadata{
					Media: []mediaInfo{{
						Part: []struct {
							Decision string `json:"decision"`
							Key      string `json:"key"`
						}{{Key: "/transcode/sessions/abc123/progress"}},
					}},
				},
			},
			key:  "abc123",
			want: true,
		},
		{
			name: "no match",
			sess: session{
				meta: sessionMetadata{
					Media: []mediaInfo{{
						Part: []struct {
							Decision string `json:"decision"`
							Key      string `json:"key"`
						}{{Key: "/transcode/sessions/xyz/progress"}},
					}},
				},
			},
			key:  "abc123",
			want: false,
		},
		{
			name: "empty session no match",
			sess: session{},
			key:  "abc123",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesTranscode(&tt.sess, tt.key); got != tt.want {
				t.Errorf("matchesTranscode() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Tests: orDefault ---

func TestOrDefault(t *testing.T) {
	if got := orDefault("hello", "world"); got != "hello" {
		t.Errorf("orDefault(hello, world) = %q, want hello", got)
	}
	if got := orDefault("", "world"); got != "world" {
		t.Errorf("orDefault(empty, world) = %q, want world", got)
	}
}

// --- Tests: sessionTracker ---

func TestSessionTrackerUpdate(t *testing.T) {
	tracker := newSessionTracker()

	meta := &sessionMetadata{Title: "Test Movie", Type: "movie"}
	tracker.update("s1", statePlaying, meta, nil)

	tracker.mu.Lock()
	s := tracker.sessions["s1"]
	tracker.mu.Unlock()

	if s.state != statePlaying {
		t.Errorf("state = %q, want playing", s.state)
	}
	if s.meta.Title != "Test Movie" {
		t.Errorf("title = %q, want Test Movie", s.meta.Title)
	}
	if s.playStarted.IsZero() {
		t.Error("playStarted should be set")
	}
}

func TestSessionTrackerStopAccumulatesTime(t *testing.T) {
	tracker := newSessionTracker()

	meta := &sessionMetadata{
		Title: "Test",
		Media: []mediaInfo{{Bitrate: 1000}},
	}
	tracker.update("s1", statePlaying, meta, nil)
	time.Sleep(10 * time.Millisecond)
	tracker.update("s1", stateStopped, nil, nil)

	tracker.mu.Lock()
	s := tracker.sessions["s1"]
	tracker.mu.Unlock()

	if s.prevPlayedTime == 0 {
		t.Error("prevPlayedTime should be > 0 after stop")
	}
	if tracker.totalEstimatedKBits == 0 {
		t.Error("totalEstimatedKBits should be > 0")
	}
}

func TestSessionTrackerPrune(t *testing.T) {
	tracker := newSessionTracker()

	tracker.mu.Lock()
	tracker.sessions["old"] = session{
		state:      stateStopped,
		lastUpdate: time.Now().Add(-2 * sessionTimeout),
	}
	tracker.sessions["recent"] = session{
		state:      stateStopped,
		lastUpdate: time.Now(),
	}
	tracker.sessions["playing"] = session{
		state:      statePlaying,
		lastUpdate: time.Now().Add(-2 * sessionTimeout),
	}
	tracker.mu.Unlock()

	tracker.prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.sessions["old"]; ok {
		t.Error("old stopped session should be pruned")
	}
	if _, ok := tracker.sessions["recent"]; !ok {
		t.Error("recent stopped session should be kept")
	}
	if _, ok := tracker.sessions["playing"]; !ok {
		t.Error("playing session should not be pruned regardless of age")
	}
}

func TestSessionTrackerUpdateMetadata(t *testing.T) {
	tracker := newSessionTracker()

	meta := &sessionMetadata{Title: "Original"}
	tracker.update("s1", statePlaying, meta, nil)

	newMeta := &sessionMetadata{Title: "Updated"}
	tracker.update("s1", statePlaying, newMeta, nil)

	tracker.mu.Lock()
	s := tracker.sessions["s1"]
	tracker.mu.Unlock()

	if s.meta.Title != "Updated" {
		t.Errorf("title = %q, want Updated", s.meta.Title)
	}
}

func TestSessionTrackerNilMeta(t *testing.T) {
	tracker := newSessionTracker()

	tracker.update("s1", statePlaying, nil, nil)

	tracker.mu.Lock()
	s := tracker.sessions["s1"]
	tracker.mu.Unlock()

	if s.state != statePlaying {
		t.Errorf("state = %q, want playing", s.state)
	}
}

// --- Tests: envOr ---

func TestEnvOr(t *testing.T) {
	t.Setenv("TEST_ENV_OR", "custom")
	if got := envOr("TEST_ENV_OR", "default"); got != "custom" {
		t.Errorf("envOr = %q, want custom", got)
	}
	t.Setenv("TEST_ENV_OR", "")
	if got := envOr("TEST_ENV_OR", "default"); got != "default" {
		t.Errorf("envOr = %q, want default", got)
	}
}

// --- Tests: setHealthy ---

func TestSetHealthy(t *testing.T) {
	setHealthy(true)
	if _, err := os.Stat(healthFile); err != nil {
		t.Error("health file should exist after setHealthy(true)")
	}
	setHealthy(false)
	if _, err := os.Stat(healthFile); err == nil {
		t.Error("health file should not exist after setHealthy(false)")
	}
}

// --- Tests: Describe ---

func TestDescribe(t *testing.T) {
	srv := &plexServer{sessions: newSessionTracker()}
	ch := make(chan *prometheus.Desc, 20)
	srv.Describe(ch)
	close(ch)

	var descs []*prometheus.Desc
	for d := range ch {
		descs = append(descs, d)
	}
	// 13 descriptors defined in the var block.
	if len(descs) != 13 {
		t.Errorf("Describe sent %d descriptors, want 13", len(descs))
	}
}

// --- Tests: Collect ---

func TestCollectServerMetrics(t *testing.T) {
	srv := &plexServer{
		name:             "TestServer",
		id:               "abc123",
		version:          "1.40.0",
		platform:         "Linux",
		platformVersion:  "6.1",
		plexPass:         true,
		hostCPU:          0.42,
		hostMem:          0.65,
		transmitBytes:    12345,
		activeTranscodes: 2,
		wsConnected:      true,
		libraries: []library{
			{ID: "1", Name: "Movies", Type: libMovie, DurationTotal: 1000, StorageTotal: 2000, ItemsCount: 50},
			{ID: "2", Name: "TV Shows", Type: libShow, DurationTotal: 3000, StorageTotal: 4000, ItemsCount: 0},
		},
		sessions: newSessionTracker(),
	}

	ch := make(chan prometheus.Metric, 50)
	srv.Collect(ch)
	close(ch)

	metrics := drainMetrics(ch)
	// 12 metrics: server_info, cpu, mem, transmit, active_transcodes, ws_connected,
	// 2x lib_duration, 2x lib_storage, 1x lib_items (only Movies has count>0), est_transmit
	if len(metrics) != 12 {
		t.Errorf("Collect produced %d metrics, want 12", len(metrics))
		for i, m := range metrics {
			t.Logf("  [%d] %s", i, m.Desc().String())
		}
	}
}

func TestCollectWithPlexPassFalse(t *testing.T) {
	srv := &plexServer{
		name:     "Srv",
		id:       "id1",
		plexPass: false,
		sessions: newSessionTracker(),
	}

	ch := make(chan prometheus.Metric, 50)
	srv.Collect(ch)
	close(ch)

	metrics := drainMetrics(ch)
	// 7 metrics: server_info, cpu, mem, transmit, active_transcodes, ws_connected, est_transmit
	if len(metrics) != 7 {
		t.Errorf("Collect produced %d metrics, want 7", len(metrics))
	}
}

// --- Tests: collectSessions ---

func TestCollectSessionsPlaying(t *testing.T) {
	tracker := newSessionTracker()
	meta := testMeta(t, `{
		"Player":{"device":"Chrome","product":"Plex Web","local":true},
		"Session":{"location":"lan","bandwidth":5000},
		"User":{"title":"testuser"},
		"Media":[{"videoResolution":"1080","bitrate":8000,"Part":[{"decision":"copy"}]}]
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Test Movie","Media":[{"videoResolution":"1080"}]}`)
	tracker.sessions["s1"] = session{
		playStarted:    time.Now().Add(-10 * time.Second),
		lastUpdate:     time.Now(),
		state:          statePlaying,
		libName:        "Movies",
		libID:          "1",
		libType:        libMovie,
		meta:           meta,
		mediaMeta:      mediaMeta,
		transcodeType:  valNone,
		subtitleAction: valNone,
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	metrics := drainMetrics(ch)
	// 4 metrics: play_count, play_seconds, session_bandwidth, est_transmit
	if len(metrics) != 4 {
		t.Errorf("collectSessions produced %d metrics, want 4", len(metrics))
	}
}

func TestCollectSessionsSkipsZeroPlayStarted(t *testing.T) {
	tracker := newSessionTracker()
	tracker.sessions["s1"] = session{
		state:      statePlaying,
		lastUpdate: time.Now(),
		// playStarted is zero — should be skipped
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	metrics := drainMetrics(ch)
	// Only est_transmit(1) — session skipped due to zero playStarted
	if len(metrics) != 1 {
		t.Errorf("collectSessions produced %d metrics, want 1 (est_transmit only)", len(metrics))
	}
}

func TestCollectSessionsLibraryLookup(t *testing.T) {
	tracker := newSessionTracker()
	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user1"}}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Resolved Movie","librarySectionID":"3"}`)
	tracker.sessions["s1"] = session{
		playStarted:    time.Now().Add(-5 * time.Second),
		lastUpdate:     time.Now(),
		state:          stateStopped,
		meta:           meta,
		mediaMeta:      mediaMeta,
		prevPlayedTime: 5 * time.Second,
	}

	libs := []library{{ID: "3", Name: "4K Movies", Type: libMovie}}
	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", libs)
	close(ch)

	metrics := drainMetrics(ch)
	// 3 metrics: play_count, play_seconds, est_transmit
	if len(metrics) != 3 {
		t.Errorf("collectSessions produced %d metrics, want 3", len(metrics))
	}
}

func TestCollectSessionsUnknownLibrary(t *testing.T) {
	tracker := newSessionTracker()
	meta := testMeta(t, `{"Player":{"device":"Phone"},"User":{"title":"user2"}}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Unknown Lib Movie","librarySectionID":"999"}`)
	tracker.sessions["s1"] = session{
		playStarted: time.Now().Add(-1 * time.Second),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		meta:        meta,
		mediaMeta:   mediaMeta,
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	metrics := drainMetrics(ch)
	// 3 metrics: play_count, play_seconds, est_transmit
	if len(metrics) != 3 {
		t.Errorf("collectSessions produced %d metrics, want 3", len(metrics))
	}
}

func TestCollectSessionsPendingTranscodeNormalized(t *testing.T) {
	tracker := newSessionTracker()
	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user3"}}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Pending Movie"}`)
	tracker.sessions["s1"] = session{
		playStarted:   time.Now().Add(-1 * time.Second),
		lastUpdate:    time.Now(),
		state:         statePlaying,
		transcodeType: valPending, // should be normalized to "none"
		libName:       "Movies",
		libID:         "1",
		libType:       libMovie,
		meta:          meta,
		mediaMeta:     mediaMeta,
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	metrics := drainMetrics(ch)
	if len(metrics) < 2 {
		t.Errorf("collectSessions produced %d metrics, want at least 2", len(metrics))
	}
}

func TestCollectSessionsEstimatedTransmitAccumulates(t *testing.T) {
	tracker := newSessionTracker()
	tracker.totalEstimatedKBits = 1000

	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user4"},"Media":[{"bitrate":5000}]}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Bitrate Movie"}`)
	tracker.sessions["s1"] = session{
		playStarted: time.Now().Add(-10 * time.Second),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		libName:     "Movies",
		libID:       "1",
		libType:     libMovie,
		meta:        meta,
		mediaMeta:   mediaMeta,
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	metrics := drainMetrics(ch)
	if len(metrics) < 3 {
		t.Errorf("collectSessions produced %d metrics, want at least 3", len(metrics))
	}
}

// --- Tests: handleTranscodeUpdate ---

func TestHandleTranscodeUpdateMatchesPending(t *testing.T) {
	tracker := newSessionTracker()
	tracker.sessions["s1"] = session{
		playStarted:   time.Now(),
		lastUpdate:    time.Now(),
		state:         statePlaying,
		transcodeType: valPending,
	}

	srv := &plexServer{
		name:     "Srv",
		id:       "id1",
		sessions: tracker,
	}

	notif := wsNotification{}
	notif.NotificationContainer.Type = "transcodeSession.update"
	notif.NotificationContainer.TranscodeSession = []wsTranscodeSession{{
		Key:           "tc1",
		VideoDecision: "transcode",
		AudioDecision: "copy",
	}}

	srv.handleTranscodeUpdate(notif)

	tracker.mu.Lock()
	s := tracker.sessions["s1"]
	tracker.mu.Unlock()

	if s.transcodeType != "video" {
		t.Errorf("transcodeType = %q, want video", s.transcodeType)
	}
	// subtitleAction: empty SubtitleDecision + VideoDecision="transcode" → "burn"
	if s.subtitleAction != valBurn {
		t.Errorf("subtitleAction = %q, want burn", s.subtitleAction)
	}
}

func TestHandleTranscodeUpdateMatchesByPartKey(t *testing.T) {
	tracker := newSessionTracker()
	tracker.sessions["s1"] = session{
		playStarted: time.Now(),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		meta: sessionMetadata{
			Media: []mediaInfo{{
				Part: []struct {
					Decision string `json:"decision"`
					Key      string `json:"key"`
				}{{Key: "/transcode/sessions/tc42/progress"}},
			}},
		},
	}

	srv := &plexServer{
		name:     "Srv",
		id:       "id1",
		sessions: tracker,
	}

	notif := wsNotification{}
	notif.NotificationContainer.TranscodeSession = []wsTranscodeSession{{
		Key:              "tc42",
		VideoDecision:    "transcode",
		AudioDecision:    "transcode",
		SubtitleDecision: "burn",
	}}

	srv.handleTranscodeUpdate(notif)

	tracker.mu.Lock()
	s := tracker.sessions["s1"]
	tracker.mu.Unlock()

	if s.transcodeType != "both" {
		t.Errorf("transcodeType = %q, want both", s.transcodeType)
	}
	if s.subtitleAction != valBurn {
		t.Errorf("subtitleAction = %q, want burn", s.subtitleAction)
	}
}

func TestHandleTranscodeUpdateNoMatch(t *testing.T) {
	tracker := newSessionTracker()
	tracker.sessions["s1"] = session{
		playStarted:   time.Now(),
		lastUpdate:    time.Now(),
		state:         statePlaying,
		transcodeType: "video",
	}

	srv := &plexServer{
		name:     "Srv",
		id:       "id1",
		sessions: tracker,
	}

	notif := wsNotification{}
	notif.NotificationContainer.TranscodeSession = []wsTranscodeSession{{
		Key:           "tc_unknown",
		VideoDecision: "transcode",
	}}

	srv.handleTranscodeUpdate(notif)

	tracker.mu.Lock()
	s := tracker.sessions["s1"]
	tracker.mu.Unlock()

	// Should remain unchanged since no match
	if s.transcodeType != "video" {
		t.Errorf("transcodeType = %q, want video (unchanged)", s.transcodeType)
	}
}

func TestHandleTranscodeUpdateMultipleSessions(t *testing.T) {
	tracker := newSessionTracker()
	tracker.sessions["s1"] = session{
		playStarted:   time.Now(),
		lastUpdate:    time.Now(),
		state:         statePlaying,
		transcodeType: valPending,
	}
	tracker.sessions["s2"] = session{
		playStarted: time.Now(),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		meta: sessionMetadata{
			Media: []mediaInfo{{
				Part: []struct {
					Decision string `json:"decision"`
					Key      string `json:"key"`
				}{{Key: "/transcode/sessions/tc99/data"}},
			}},
		},
	}

	srv := &plexServer{
		name:     "Srv",
		id:       "id1",
		sessions: tracker,
	}

	// First transcode update matches s1 (pending)
	notif := wsNotification{}
	notif.NotificationContainer.TranscodeSession = []wsTranscodeSession{{
		Key:           "tc_first",
		AudioDecision: "transcode",
	}}
	srv.handleTranscodeUpdate(notif)

	tracker.mu.Lock()
	s1 := tracker.sessions["s1"]
	tracker.mu.Unlock()

	if s1.transcodeType != "audio" {
		t.Errorf("s1 transcodeType = %q, want audio", s1.transcodeType)
	}

	// Second transcode update matches s2 by part key
	notif2 := wsNotification{}
	notif2.NotificationContainer.TranscodeSession = []wsTranscodeSession{{
		Key:           "tc99",
		VideoDecision: "transcode",
		AudioDecision: "copy",
	}}
	srv.handleTranscodeUpdate(notif2)

	tracker.mu.Lock()
	s2 := tracker.sessions["s2"]
	tracker.mu.Unlock()

	if s2.transcodeType != "video" {
		t.Errorf("s2 transcodeType = %q, want video", s2.transcodeType)
	}
}

// --- Tests: Collect with sessions (full integration of Collect → collectSessions) ---

func TestCollectWithActiveSessions(t *testing.T) {
	tracker := newSessionTracker()
	meta := testMeta(t, `{
		"Player":{"device":"TV","product":"Plex for LG","local":true},
		"Session":{"location":"lan","bandwidth":10000},
		"User":{"title":"admin"},
		"Media":[{"videoResolution":"2160","bitrate":20000,"Part":[{"decision":"transcode"}]}]
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"4K Movie","Media":[{"videoResolution":"2160"}]}`)
	tracker.sessions["s1"] = session{
		playStarted:    time.Now().Add(-5 * time.Second),
		lastUpdate:     time.Now(),
		state:          statePlaying,
		libName:        "Movies",
		libID:          "1",
		libType:        libMovie,
		meta:           meta,
		mediaMeta:      mediaMeta,
		transcodeType:  "video",
		subtitleAction: valBurn,
	}

	srv := &plexServer{
		name:             "TestPlex",
		id:               "plex1",
		version:          "1.40.0",
		platform:         "Linux",
		platformVersion:  "6.1",
		plexPass:         true,
		hostCPU:          0.5,
		hostMem:          0.7,
		transmitBytes:    50000,
		activeTranscodes: 1,
		wsConnected:      true,
		libraries: []library{
			{ID: "1", Name: "Movies", Type: libMovie, DurationTotal: 5000, StorageTotal: 10000, ItemsCount: 100},
		},
		sessions: tracker,
	}

	ch := make(chan prometheus.Metric, 50)
	srv.Collect(ch)
	close(ch)

	metrics := drainMetrics(ch)
	// 13 metrics: server_info, cpu, mem, transmit, active_transcodes, ws_connected,
	// lib_duration, lib_storage, lib_items, play_count, play_seconds, session_bandwidth, est_transmit
	if len(metrics) != 13 {
		t.Errorf("Collect with sessions produced %d metrics, want 13", len(metrics))
	}
}

// --- Tests: sessionTracker edge cases ---

func TestSessionTrackerResumeAfterStop(t *testing.T) {
	tracker := newSessionTracker()

	meta := &sessionMetadata{
		Title: "Resume Test",
		Media: []mediaInfo{{Bitrate: 2000}},
	}
	tracker.update("s1", statePlaying, meta, nil)
	time.Sleep(10 * time.Millisecond)
	tracker.update("s1", stateStopped, nil, nil)

	tracker.mu.Lock()
	prev := tracker.sessions["s1"].prevPlayedTime
	tracker.mu.Unlock()

	// Resume playing
	tracker.update("s1", statePlaying, nil, nil)
	time.Sleep(10 * time.Millisecond)
	tracker.update("s1", stateStopped, nil, nil)

	tracker.mu.Lock()
	after := tracker.sessions["s1"].prevPlayedTime
	tracker.mu.Unlock()

	if after <= prev {
		t.Errorf("prevPlayedTime should accumulate: before=%v, after=%v", prev, after)
	}
}

func TestSessionTrackerMediaMetaUpdate(t *testing.T) {
	tracker := newSessionTracker()

	meta := &sessionMetadata{Title: "Session"}
	mediaMeta := &sessionMetadata{Title: "Media Info", Type: "movie"}
	tracker.update("s1", statePlaying, meta, mediaMeta)

	tracker.mu.Lock()
	s := tracker.sessions["s1"]
	tracker.mu.Unlock()

	if s.mediaMeta.Title != "Media Info" {
		t.Errorf("mediaMeta.Title = %q, want Media Info", s.mediaMeta.Title)
	}
	if s.mediaMeta.Type != "movie" {
		t.Errorf("mediaMeta.Type = %q, want movie", s.mediaMeta.Type)
	}
}

// drainMetrics collects all metrics from a closed channel.
func drainMetrics(ch <-chan prometheus.Metric) []prometheus.Metric {
	var result []prometheus.Metric
	for m := range ch {
		result = append(result, m)
	}
	return result
}

// --- Tests: newPlexServer ---

func TestNewPlexServer(t *testing.T) {
	client := &plexClient{}
	srv := newPlexServer(client)
	if srv.client != client {
		t.Error("client not set")
	}
	if srv.sessions == nil {
		t.Error("sessions tracker not initialized")
	}
	if srv.lastBandwidthAt == 0 {
		t.Error("lastBandwidthAt should be initialized to current time")
	}
}

// --- Tests: runPruneLoop ---

func TestRunPruneLoopCancellation(t *testing.T) {
	tracker := newSessionTracker()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	done := make(chan struct{})
	go func() {
		tracker.runPruneLoop(ctx)
		close(done)
	}()

	select {
	case <-done:
		// ok — loop exited on cancelled context
	case <-time.After(time.Second):
		t.Fatal("runPruneLoop did not exit on cancelled context")
	}
}

// --- Tests: collectSessions with episode/track types ---

func TestCollectSessionsEpisodeLabels(t *testing.T) {
	tracker := newSessionTracker()
	meta := testMeta(t, `{"Player":{"device":"Roku"},"User":{"title":"viewer"}}`)
	mediaMeta := testMeta(t, `{
		"type":"episode",
		"grandparentTitle":"Breaking Bad",
		"parentTitle":"Season 1",
		"title":"Pilot"
	}`)
	tracker.sessions["s1"] = session{
		playStarted: time.Now().Add(-3 * time.Second),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		libName:     "TV Shows",
		libID:       "2",
		libType:     libShow,
		meta:        meta,
		mediaMeta:   mediaMeta,
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	metrics := drainMetrics(ch)
	// 3 metrics: play_count, play_seconds, est_transmit
	if len(metrics) != 3 {
		t.Errorf("collectSessions episode produced %d metrics, want 3", len(metrics))
	}
}

func TestCollectSessionsMultipleSessions(t *testing.T) {
	tracker := newSessionTracker()
	meta1 := testMeta(t, `{
		"Player":{"device":"TV","local":true},
		"Session":{"location":"lan","bandwidth":8000},
		"User":{"title":"user1"},
		"Media":[{"bitrate":10000}]
	}`)
	meta2 := testMeta(t, `{
		"Player":{"device":"Phone"},
		"User":{"title":"user2"},
		"Media":[{"bitrate":3000}]
	}`)
	tracker.sessions["s1"] = session{
		playStarted: time.Now().Add(-5 * time.Second),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		libName:     "Movies",
		libID:       "1",
		libType:     libMovie,
		meta:        meta1,
		mediaMeta:   testMeta(t, `{"type":"movie","title":"Movie A"}`),
	}
	tracker.sessions["s2"] = session{
		playStarted: time.Now().Add(-2 * time.Second),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		libName:     "Movies",
		libID:       "1",
		libType:     libMovie,
		meta:        meta2,
		mediaMeta:   testMeta(t, `{"type":"movie","title":"Movie B"}`),
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 30)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	metrics := drainMetrics(ch)
	// 6 metrics: s1 gets play_count, play_seconds, session_bandwidth;
	// s2 gets play_count, play_seconds; plus est_transmit
	if len(metrics) != 6 {
		t.Errorf("collectSessions multi produced %d metrics, want 6", len(metrics))
	}
}

// --- Tests: Collect with multiple libraries ---

func TestCollectMultipleLibraries(t *testing.T) {
	srv := &plexServer{
		name:    "Srv",
		id:      "id1",
		version: "1.0",
		libraries: []library{
			{ID: "1", Name: "Movies", Type: libMovie, DurationTotal: 100, StorageTotal: 200, ItemsCount: 10},
			{ID: "2", Name: "TV", Type: libShow, DurationTotal: 300, StorageTotal: 400, ItemsCount: 20},
			{ID: "3", Name: "Music", Type: libArtist, DurationTotal: 500, StorageTotal: 600, ItemsCount: 30},
		},
		sessions: newSessionTracker(),
	}

	ch := make(chan prometheus.Metric, 50)
	srv.Collect(ch)
	close(ch)

	metrics := drainMetrics(ch)
	// 16 metrics: 6 server-level, 3x lib_duration, 3x lib_storage, 3x lib_items, est_transmit
	if len(metrics) != 16 {
		t.Errorf("Collect multi-lib produced %d metrics, want 16", len(metrics))
	}
}

// --- Tests: plexClient HTTP methods ---

func TestGetWithHeaders(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "test-token" {
			t.Error("missing plex token header")
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing accept header")
		}
		switch r.URL.Path {
		case "/test":
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"TestPlex"}}`)
		case "/notfound":
			w.WriteHeader(http.StatusNotFound)
		case "/error":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	parsed, _ := url.Parse(srv.URL)
	client := &plexClient{
		httpClient: srv.Client(),
		baseURL:    parsed,
		token:      "test-token",
	}

	t.Run("success", func(t *testing.T) {
		var resp mc[serverIdentity]
		err := client.get(context.Background(), "/test", &resp)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.MediaContainer.FriendlyName != "TestPlex" {
			t.Errorf("name = %q, want TestPlex", resp.MediaContainer.FriendlyName)
		}
	})

	t.Run("not found", func(t *testing.T) {
		var resp mc[serverIdentity]
		err := client.get(context.Background(), "/notfound", &resp)
		if !errors.Is(err, errNotFound) {
			t.Errorf("expected errNotFound, got %v", err)
		}
	})

	t.Run("server error", func(t *testing.T) {
		var resp mc[serverIdentity]
		err := client.get(context.Background(), "/error", &resp)
		if err == nil {
			t.Fatal("expected error for 500 response")
		}
		if errors.Is(err, errNotFound) {
			t.Error("should not be errNotFound")
		}
	})

	t.Run("extra headers", func(t *testing.T) {
		var resp mc[serverIdentity]
		err := client.getWithHeaders(context.Background(), "/test", &resp, map[string]string{
			"X-Custom": "value",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestGetContainerSize(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Container-Start") != "0" {
			t.Error("missing container start header")
		}
		if r.Header.Get("X-Plex-Container-Size") != "1" {
			t.Error("missing container size header")
		}
		fmt.Fprint(w, `{"MediaContainer":{"totalSize":42}}`)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	parsed, _ := url.Parse(srv.URL)
	client := &plexClient{
		httpClient: srv.Client(),
		baseURL:    parsed,
		token:      "test-token",
	}

	size, err := client.getContainerSize(context.Background(), "/library/sections/1/all")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if size != 42 {
		t.Errorf("size = %d, want 42", size)
	}
}

func TestGetWithRetry(t *testing.T) {
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"RetryPlex"}}`)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	parsed, _ := url.Parse(srv.URL)
	client := &plexClient{
		httpClient: srv.Client(),
		baseURL:    parsed,
		token:      "test-token",
	}

	var resp mc[serverIdentity]
	err := client.getWithRetry(context.Background(), "/", &resp, 3)
	if err != nil {
		t.Fatalf("unexpected error after retries: %v", err)
	}
	if resp.MediaContainer.FriendlyName != "RetryPlex" {
		t.Errorf("name = %q, want RetryPlex", resp.MediaContainer.FriendlyName)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestGetWithRetryExhausted(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	parsed, _ := url.Parse(srv.URL)
	client := &plexClient{
		httpClient: srv.Client(),
		baseURL:    parsed,
		token:      "test-token",
	}

	var resp mc[serverIdentity]
	err := client.getWithRetry(context.Background(), "/", &resp, 2)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
}

func TestGetWithRetryCancellation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	parsed, _ := url.Parse(srv.URL)
	client := &plexClient{
		httpClient: srv.Client(),
		baseURL:    parsed,
		token:      "test-token",
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	var resp mc[serverIdentity]
	err := client.getWithRetry(ctx, "/", &resp, 5)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
}

// ---------------------------------------------------------------------------
// Property-based tests (rapid)
// ---------------------------------------------------------------------------

func TestTranscodeKind_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ts := &wsTranscodeSession{
			VideoDecision:    rapid.String().Draw(t, "videoDecision"),
			AudioDecision:    rapid.String().Draw(t, "audioDecision"),
			SourceVideoCodec: rapid.String().Draw(t, "srcVideo"),
			SourceAudioCodec: rapid.String().Draw(t, "srcAudio"),
			VideoCodec:       rapid.String().Draw(t, "videoCodec"),
			AudioCodec:       rapid.String().Draw(t, "audioCodec"),
		}
		got := transcodeKind(ts)
		valid := map[string]bool{"video": true, "audio": true, "both": true, valNone: true}
		if !valid[got] {
			t.Errorf("transcodeKind() = %q, not in valid set", got)
		}
	})
}

func TestSubtitleAction_never_panics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ts := &wsTranscodeSession{
			SubtitleDecision: rapid.String().Draw(t, "subtitleDecision"),
			Container:        rapid.String().Draw(t, "container"),
			VideoDecision:    rapid.String().Draw(t, "videoDecision"),
		}
		got := subtitleAction(ts)
		if got == "" {
			t.Error("subtitleAction() returned empty string")
		}
	})
}

func TestTranscodeKind_video_decision_dominates(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ts := &wsTranscodeSession{
			VideoDecision: "transcode",
			AudioDecision: rapid.SampledFrom([]string{"copy", "direct play", ""}).Draw(t, "audio"),
		}
		got := transcodeKind(ts)
		if got != "video" && got != "both" {
			t.Errorf("transcodeKind() with video=transcode = %q, want video or both", got)
		}
	})
}

func TestTranscodeKind_audio_decision_dominates(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ts := &wsTranscodeSession{
			VideoDecision: rapid.SampledFrom([]string{"copy", "direct play", ""}).Draw(t, "video"),
			AudioDecision: "transcode",
		}
		got := transcodeKind(ts)
		if got != "audio" && got != "both" {
			t.Errorf("transcodeKind() with audio=transcode = %q, want audio or both", got)
		}
	})
}

func TestMatchesTranscode_pending_always_matches(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		key := rapid.String().Draw(t, "key")
		ss := &session{transcodeType: valPending}
		if !matchesTranscode(ss, key) {
			t.Errorf("matchesTranscode(pending, %q) = false, want true", key)
		}
	})
}

func TestSessionLabels_movie_returns_title_only(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		title := rapid.String().Draw(t, "title")
		m := &sessionMetadata{Type: "movie", Title: title}
		gotTitle, gotChild, gotGC := sessionLabels(m)
		if gotTitle != title {
			t.Errorf("sessionLabels(movie) title = %q, want %q", gotTitle, title)
		}
		if gotChild != "" {
			t.Errorf("sessionLabels(movie) child = %q, want empty", gotChild)
		}
		if gotGC != "" {
			t.Errorf("sessionLabels(movie) grandchild = %q, want empty", gotGC)
		}
	})
}

func TestSessionLabels_episode_returns_hierarchy(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		gp := rapid.String().Draw(t, "grandparent")
		p := rapid.String().Draw(t, "parent")
		title := rapid.String().Draw(t, "title")
		m := &sessionMetadata{
			Type:             "episode",
			GrandparentTitle: gp,
			ParentTitle:      p,
			Title:            title,
		}
		gotTitle, gotChild, gotGC := sessionLabels(m)
		if gotTitle != gp {
			t.Errorf("sessionLabels(episode) title = %q, want %q", gotTitle, gp)
		}
		if gotChild != p {
			t.Errorf("sessionLabels(episode) child = %q, want %q", gotChild, p)
		}
		if gotGC != title {
			t.Errorf("sessionLabels(episode) grandchild = %q, want %q", gotGC, title)
		}
	})
}

func TestOrDefault_non_empty_returns_input(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.StringMatching(".+").Draw(t, "input")
		def := rapid.String().Draw(t, "default")
		got := orDefault(s, def)
		if got != s {
			t.Errorf("orDefault(%q, %q) = %q, want %q", s, def, got, s)
		}
	})
}

func TestOrDefault_empty_returns_default(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		def := rapid.String().Draw(t, "default")
		got := orDefault("", def)
		if got != def {
			t.Errorf("orDefault(\"\", %q) = %q, want %q", def, got, def)
		}
	})
}

func TestIsLibraryType_valid_types_exhaustive(t *testing.T) {
	// Verify the exact set of valid types
	validTypes := []string{"movie", "show", "artist", "photo", "homevideo"}
	for _, v := range validTypes {
		if !isLibraryType(v) {
			t.Errorf("isLibraryType(%q) = false, want true", v)
		}
	}
}

func TestIsLibraryType_random_strings_mostly_false(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "type")
		got := isLibraryType(s)
		valid := map[string]bool{"movie": true, "show": true, "artist": true, "photo": true, "homevideo": true}
		if got != valid[s] {
			t.Errorf("isLibraryType(%q) = %v, want %v", s, got, valid[s])
		}
	})
}

func TestContentTypeLabel_always_returns_non_empty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := rapid.String().Draw(t, "libType")
		got := contentTypeLabel(s)
		if got == "" {
			t.Errorf("contentTypeLabel(%q) returned empty string", s)
		}
	})
}

// ---------------------------------------------------------------------------
// HTTP mock tests for refresh functions
// ---------------------------------------------------------------------------

// newTestPlexClient creates a plexClient pointing at the given test server.
func newTestPlexClient(t *testing.T, srv *httptest.Server) *plexClient {
	t.Helper()
	parsed, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	return &plexClient{
		httpClient: srv.Client(),
		baseURL:    parsed,
		token:      "test-token",
	}
}

func TestRefreshResources_updates_host_metrics(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/resources" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[
				{"hostCpuUtilization":25.0,"hostMemoryUtilization":50.0},
				{"hostCpuUtilization":42.0,"hostMemoryUtilization":65.0}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.refreshResources(context.Background())

	server.mu.Lock()
	cpu := server.hostCPU
	mem := server.hostMem
	server.mu.Unlock()

	// API returns percentages (0-100), code divides by 100 to get ratios
	if cpu != 0.42 {
		t.Errorf("hostCPU = %v, want 0.42", cpu)
	}
	if mem != 0.65 {
		t.Errorf("hostMem = %v, want 0.65", mem)
	}
}

func TestRefreshResources_empty_stats_no_update(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/resources" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.hostCPU = 0.99
	server.refreshResources(context.Background())

	server.mu.Lock()
	cpu := server.hostCPU
	server.mu.Unlock()

	if cpu != 0.99 {
		t.Errorf("hostCPU = %v, want 0.99 (unchanged)", cpu)
	}
}

func TestRefreshResources_404_no_update(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.hostCPU = 0.11
	server.hostMem = 0.22
	server.refreshResources(context.Background())

	server.mu.Lock()
	cpu := server.hostCPU
	mem := server.hostMem
	server.mu.Unlock()

	if cpu != 0.11 {
		t.Errorf("hostCPU = %v, want 0.11 (unchanged after 404)", cpu)
	}
	if mem != 0.22 {
		t.Errorf("hostMem = %v, want 0.22 (unchanged after 404)", mem)
	}
}

func TestRefreshBandwidth_accumulates_bytes(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
				{"bytes":100,"at":1000},
				{"bytes":200,"at":2000},
				{"bytes":300,"at":3000}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.lastBandwidthAt = 1500

	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	transmit := server.transmitBytes
	lastAt := server.lastBandwidthAt
	server.mu.Unlock()

	// Only entries with at > 1500 should be counted: at=2000 (200) + at=3000 (300) = 500
	if transmit != 500 {
		t.Errorf("transmitBytes = %v, want 500", transmit)
	}
	if lastAt != 3000 {
		t.Errorf("lastBandwidthAt = %d, want 3000", lastAt)
	}
}

func TestRefreshBandwidth_404_no_update(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.transmitBytes = 999
	server.lastBandwidthAt = 42

	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	transmit := server.transmitBytes
	lastAt := server.lastBandwidthAt
	server.mu.Unlock()

	if transmit != 999 {
		t.Errorf("transmitBytes = %v, want 999 (unchanged)", transmit)
	}
	if lastAt != 42 {
		t.Errorf("lastBandwidthAt = %d, want 42 (unchanged)", lastAt)
	}
}

func TestRefreshBandwidth_empty_stats(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.lastBandwidthAt = 100

	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	transmit := server.transmitBytes
	server.mu.Unlock()

	if transmit != 0 {
		t.Errorf("transmitBytes = %v, want 0", transmit)
	}
}

func TestRefresh_populates_server_state(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"TestPlex","machineIdentifier":"abc123","version":"1.40.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie","durationTotal":1000,"storageTotal":2000},
						{"title":"TV Shows","id":"2","type":"show","durationTotal":3000,"storageTotal":4000},
						{"title":"Playlists","id":"3","type":"playlist","durationTotal":0,"storageTotal":0}
					]}
				]}]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"TestPlex","machineIdentifier":"abc123",
				"version":"1.40.0","platform":"Linux","platformVersion":"6.1",
				"myPlexSubscription":true,"transcoderActiveVideoSessions":2
			}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[
				{"hostCpuUtilization":50.0,"hostMemoryUtilization":70.0}
			]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	err := server.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	if server.name != "TestPlex" {
		t.Errorf("name = %q, want TestPlex", server.name)
	}
	if server.id != "abc123" {
		t.Errorf("id = %q, want abc123", server.id)
	}
	if server.platform != "Linux" {
		t.Errorf("platform = %q, want Linux", server.platform)
	}
	if !server.plexPass {
		t.Error("plexPass = false, want true")
	}
	if server.activeTranscodes != 2 {
		t.Errorf("activeTranscodes = %d, want 2", server.activeTranscodes)
	}
	// Should have 2 libraries (movie + show), not playlist
	if len(server.libraries) != 2 {
		t.Errorf("libraries count = %d, want 2", len(server.libraries))
	}
}

func TestRefresh_preserves_item_counts(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie","durationTotal":100,"storageTotal":200}
					]}
				]}]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"platform":"Linux","platformVersion":"6.1"
			}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	// Pre-populate with item counts
	server.libraries = []library{
		{ID: "1", Name: "Movies", Type: libMovie, ItemsCount: 500},
	}
	// Set lastItemsRefresh to recent so it won't re-fetch items
	server.lastItemsRefresh = time.Now()

	err := server.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	if len(server.libraries) != 1 {
		t.Fatalf("libraries count = %d, want 1", len(server.libraries))
	}
	if server.libraries[0].ItemsCount != 500 {
		t.Errorf("ItemsCount = %d, want 500 (preserved)", server.libraries[0].ItemsCount)
	}
}

func TestRefresh_filters_non_library_providers(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[
					{"identifier":"com.plexapp.plugins.library","Feature":[
						{"type":"content","Directory":[
							{"title":"Movies","id":"1","type":"movie"}
						]},
						{"type":"timeline","Directory":[
							{"title":"Timeline","id":"99","type":"movie"}
						]}
					]},
					{"identifier":"tv.plex.provider.vod","Feature":[
						{"type":"content","Directory":[
							{"title":"VOD","id":"50","type":"movie"}
						]}
					]}
				]
			}}`)
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0"}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	err := server.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	// Only 1 library: Movies from com.plexapp.plugins.library content feature
	if len(server.libraries) != 1 {
		t.Errorf("libraries count = %d, want 1", len(server.libraries))
	}
}

func TestRefresh_provider_error_returns_error(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	err := server.refresh(context.Background())
	if err == nil {
		t.Fatal("refresh() should return error on provider failure")
	}
}

func TestRefreshLibraryItems_counts_by_type(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/sections/1/all":
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":150}}`)
		case "/library/sections/2/all":
			// Show library with type=4 (episodes)
			if r.URL.Query().Get("type") == "4" {
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":500}}`)
				return
			}
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":25}}`)
		case "/library/sections/3/all":
			// Artist library: type=10 (tracks) first
			if r.URL.Query().Get("type") == "10" {
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":2000}}`)
				return
			}
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":100}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.libraries = []library{
		{ID: "1", Name: "Movies", Type: libMovie},
		{ID: "2", Name: "TV Shows", Type: libShow},
		{ID: "3", Name: "Music", Type: libArtist},
	}

	server.refreshLibraryItems(context.Background())

	server.mu.Lock()
	defer server.mu.Unlock()

	if server.libraries[0].ItemsCount != 150 {
		t.Errorf("Movies ItemsCount = %d, want 150", server.libraries[0].ItemsCount)
	}
	if server.libraries[1].ItemsCount != 500 {
		t.Errorf("TV Shows ItemsCount = %d, want 500 (episodes)", server.libraries[1].ItemsCount)
	}
	if server.libraries[2].ItemsCount != 2000 {
		t.Errorf("Music ItemsCount = %d, want 2000 (tracks)", server.libraries[2].ItemsCount)
	}
}

func TestRefreshLibraryItems_artist_fallback_to_type7(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				// type=10 returns 0 — trigger fallback
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":0}}`)
			case "7":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":350}}`)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":50}}`)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.libraries = []library{
		{ID: "1", Name: "Music", Type: libArtist},
	}

	server.refreshLibraryItems(context.Background())

	server.mu.Lock()
	defer server.mu.Unlock()

	if server.libraries[0].ItemsCount != 350 {
		t.Errorf("Music ItemsCount = %d, want 350 (type=7 fallback)", server.libraries[0].ItemsCount)
	}
}

// ---------------------------------------------------------------------------
// Tests: handlePlaying (HTTP mock)
// ---------------------------------------------------------------------------

func TestHandlePlaying_updates_session_from_api(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"Chrome","product":"Plex Web","state":"playing","local":true},
				"Session":{"location":"lan","bandwidth":5000},
				"User":{"title":"testuser","id":"1"},
				"Media":[{"videoResolution":"1080","bitrate":8000,"Part":[{"decision":"copy"}]}]
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"Test Movie",
				"librarySectionID":"1",
				"Media":[{"videoResolution":"1080"}]
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.libraries = []library{
		{ID: "1", Name: "Movies", Type: libMovie},
	}

	notif := wsNotification{}
	notif.NotificationContainer.Type = "playing"
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	s, ok := server.sessions.sessions["s1"]
	server.sessions.mu.Unlock()

	if !ok {
		t.Fatal("session s1 not found")
	}
	if s.state != statePlaying {
		t.Errorf("state = %q, want playing", s.state)
	}
	if s.mediaMeta.Title != "Test Movie" {
		t.Errorf("mediaMeta.Title = %q, want Test Movie", s.mediaMeta.Title)
	}
	if s.libName != "Movies" {
		t.Errorf("libName = %q, want Movies", s.libName)
	}
	if s.libID != "1" {
		t.Errorf("libID = %q, want 1", s.libID)
	}
}

func TestHandlePlaying_stopped_updates_without_api(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	// Pre-populate a playing session
	meta := &sessionMetadata{Title: "Playing Movie", Media: []mediaInfo{{Bitrate: 5000}}}
	server.sessions.update("s1", statePlaying, meta, nil)

	notif := wsNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "stopped",
	}}

	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	s := server.sessions.sessions["s1"]
	server.sessions.mu.Unlock()

	if s.state != stateStopped {
		t.Errorf("state = %q, want stopped", s.state)
	}
}

func TestHandlePlaying_invalid_rating_key_skipped(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"abc",
				"Player":{"device":"TV"},
				"User":{"title":"user1"}
			}]}}`)
			return
		}
		t.Errorf("unexpected request to %s (should not fetch metadata for invalid key)", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	notif := wsNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "abc",
		State:      "playing",
	}}

	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	_, ok := server.sessions.sessions["s1"]
	server.sessions.mu.Unlock()

	if ok {
		t.Error("session should not be created for invalid rating key")
	}
}

func TestHandlePlaying_session_not_in_api_skipped(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status/sessions" {
			// Return sessions but not the one we're looking for
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"other","ratingKey":"200",
				"Player":{"device":"TV"},
				"User":{"title":"user2"}
			}]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	notif := wsNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	_, ok := server.sessions.sessions["s1"]
	server.sessions.mu.Unlock()

	if ok {
		t.Error("session should not be created when not found in sessions API")
	}
}

func TestHandlePlaying_empty_metadata_response_skipped(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"TV"},
				"User":{"title":"user1"}
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	notif := wsNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	_, ok := server.sessions.sessions["s1"]
	server.sessions.mu.Unlock()

	if ok {
		t.Error("session should not be created when metadata response is empty")
	}
}

func TestHandlePlaying_transcode_session_marks_pending(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"TV"},
				"User":{"title":"user1"},
				"Media":[{"videoResolution":"1080","Part":[{"decision":"transcode"}]}]
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"Transcoded Movie","librarySectionID":"1"
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	notif := wsNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey:       "s1",
		RatingKey:        "100",
		State:            "playing",
		TranscodeSession: "tc123",
	}}

	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	s := server.sessions.sessions["s1"]
	server.sessions.mu.Unlock()

	if s.transcodeType != valPending {
		t.Errorf("transcodeType = %q, want pending", s.transcodeType)
	}
}

func TestHandlePlaying_sessions_api_failure_returns_early(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	notif := wsNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	// Should not panic, just return early
	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	count := len(server.sessions.sessions)
	server.sessions.mu.Unlock()

	if count != 0 {
		t.Errorf("sessions count = %d, want 0 (API failure)", count)
	}
}

// --- Round 1: targeting remaining partial coverage gaps ---

func TestRefresh_server_info_error_returns_error(t *testing.T) {
	// Targets uncovered line 642: when "/" endpoint fails after providers succeed.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie"}
					]}
				]}]
			}}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	err := server.refresh(context.Background())
	if err == nil {
		t.Fatal("refresh() should return error when server info fetch fails")
	}
	if !strings.Contains(err.Error(), "fetching server info") {
		t.Errorf("error = %q, want to contain 'fetching server info'", err.Error())
	}
}

func TestHandlePlaying_metadata_fetch_error_skips_session(t *testing.T) {
	// Targets uncovered line 1066-1068: when metadata fetch fails for a valid rating key.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"TV"},
				"User":{"title":"user1"}
			}]}}`)
		case "/library/metadata/100":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)

	notif := wsNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	_, ok := server.sessions.sessions["s1"]
	server.sessions.mu.Unlock()

	if ok {
		t.Error("session should not be created when metadata fetch fails")
	}
}

func TestHandlePlaying_library_not_found_uses_unknown(t *testing.T) {
	// Targets uncovered line 1096-1097: when library ID doesn't match any known library.
	// The session should still be created but with unknown library labels.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/status/sessions":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"sessionKey":"s1","ratingKey":"100",
				"Player":{"device":"TV"},
				"User":{"title":"user1"},
				"Media":[{"videoResolution":"1080","Part":[{"decision":"copy"}]}]
			}]}}`)
		case "/library/metadata/100":
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[{
				"type":"movie","title":"Orphan Movie","librarySectionID":"999"
			}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	// Only library ID "1" exists — session has librarySectionID "999"
	server.libraries = []library{
		{ID: "1", Name: "Movies", Type: libMovie},
	}

	notif := wsNotification{}
	notif.NotificationContainer.PlaySessionStateNotification = []wsPlayNotification{{
		SessionKey: "s1",
		RatingKey:  "100",
		State:      "playing",
	}}

	server.handlePlaying(context.Background(), notif)

	server.sessions.mu.Lock()
	s, ok := server.sessions.sessions["s1"]
	server.sessions.mu.Unlock()

	if !ok {
		t.Fatal("session s1 should be created")
	}
	// Library labels should remain empty (not resolved) since ID 999 doesn't match
	if s.libName != "" {
		t.Errorf("libName = %q, want empty (library not found)", s.libName)
	}
}

func TestGetWithHeaders_invalid_url_returns_error(t *testing.T) {
	// Targets uncovered line 191-193: url.Parse error path.
	parsed, _ := url.Parse("http://localhost")
	client := &plexClient{
		httpClient: &http.Client{},
		baseURL:    parsed,
		token:      "test-token",
	}

	var resp mc[serverIdentity]
	err := client.getWithHeaders(context.Background(), "://invalid", &resp, nil)
	if err == nil {
		t.Fatal("expected error for invalid URL path")
	}
}

func TestGetWithHeaders_invalid_json_returns_error(t *testing.T) {
	// Targets the json.Unmarshal error path at the end of getWithHeaders.
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `not json at all`)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)

	var resp mc[serverIdentity]
	err := client.get(context.Background(), "/test", &resp)
	if err == nil {
		t.Fatal("expected error for invalid JSON response")
	}
}

func TestSubtitleAction_srt_in_container_string(t *testing.T) {
	// Targets the strings.Contains(ctn, "srt") path when container contains "srt"
	// but isn't exactly "srt".
	ts := &wsTranscodeSession{Container: "mkv-srt-embedded"}
	got := subtitleAction(ts)
	if got != valCopy {
		t.Errorf("subtitleAction(container=mkv-srt-embedded) = %q, want copy", got)
	}
}

func TestSubtitleAction_whitespace_trimmed(t *testing.T) {
	// Verify whitespace is trimmed from SubtitleDecision.
	ts := &wsTranscodeSession{SubtitleDecision: " burn "}
	got := subtitleAction(ts)
	if got != valBurn {
		t.Errorf("subtitleAction(SubtitleDecision=' burn ') = %q, want burn", got)
	}
}

func TestSubtitleAction_case_insensitive(t *testing.T) {
	// Verify case insensitivity for SubtitleDecision.
	tests := []struct {
		input string
		want  string
	}{
		{"BURN", valBurn},
		{"Burn-In", valBurn},
		{"COPY", valCopy},
		{"Copying", valCopy},
		{"TRANSCODE", valTranscode},
		{"Transcoding", valTranscode},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ts := &wsTranscodeSession{SubtitleDecision: tt.input}
			got := subtitleAction(ts)
			if got != tt.want {
				t.Errorf("subtitleAction(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTranscodeKind_mixed_codec_change_and_decision(t *testing.T) {
	// When both video decision is "transcode" AND audio codec changes,
	// result should be "both".
	ts := &wsTranscodeSession{
		VideoDecision:    "transcode",
		SourceAudioCodec: "truehd",
		AudioCodec:       "aac",
	}
	got := transcodeKind(ts)
	if got != "both" {
		t.Errorf("transcodeKind(video=transcode + audio codec change) = %q, want both", got)
	}
}

func TestTranscodeKind_empty_codec_no_transcode(t *testing.T) {
	// When video codec is empty, it should not count as a codec change.
	ts := &wsTranscodeSession{
		SourceVideoCodec: "h264",
		VideoCodec:       "",
	}
	got := transcodeKind(ts)
	if got != valNone {
		t.Errorf("transcodeKind(empty VideoCodec) = %q, want none", got)
	}
}

func TestMatchesTranscode_empty_part_key_no_match(t *testing.T) {
	// When Part.Key is empty, it should not match any transcode key.
	ss := &session{
		meta: sessionMetadata{
			Media: []mediaInfo{{
				Part: []struct {
					Decision string `json:"decision"`
					Key      string `json:"key"`
				}{{Key: ""}},
			}},
		},
	}
	if matchesTranscode(ss, "abc") {
		t.Error("matchesTranscode should not match when Part.Key is empty")
	}
}

func TestMatchesTranscode_multiple_media_parts(t *testing.T) {
	// When there are multiple media entries and parts, should check all of them.
	ss := &session{
		meta: sessionMetadata{
			Media: []mediaInfo{
				{Part: []struct {
					Decision string `json:"decision"`
					Key      string `json:"key"`
				}{{Key: "/transcode/sessions/other/data"}}},
				{Part: []struct {
					Decision string `json:"decision"`
					Key      string `json:"key"`
				}{{Key: "/transcode/sessions/target/data"}}},
			},
		},
	}
	if !matchesTranscode(ss, "target") {
		t.Error("matchesTranscode should find key in second media entry")
	}
}
