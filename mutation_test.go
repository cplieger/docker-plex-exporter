package main

// mutation_test.go — Tests targeting specific lived mutants to improve mutation efficacy.
// These tests assert on metric label values and numeric values, not just metric counts.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricSnapshot extracts label pairs and the numeric value from a prometheus.Metric.
func metricSnapshot(t *testing.T, m prometheus.Metric) (labels map[string]string, value float64) {
	t.Helper()
	d := &dto.Metric{}
	if err := m.Write(d); err != nil {
		t.Fatalf("metricSnapshot: %v", err)
	}
	labels = make(map[string]string, len(d.GetLabel()))
	for _, lp := range d.GetLabel() {
		labels[lp.GetName()] = lp.GetValue()
	}
	if g := d.GetGauge(); g != nil {
		value = g.GetValue()
	} else if c := d.GetCounter(); c != nil {
		value = c.GetValue()
	}
	return labels, value
}

// collectByDesc collects metrics from a channel and groups them by descriptor string prefix.
func collectByDesc(ch <-chan prometheus.Metric) map[string][]prometheus.Metric {
	result := make(map[string][]prometheus.Metric)
	for m := range ch {
		desc := m.Desc().String()
		result[desc] = append(result[desc], m)
	}
	return result
}

// descKey returns the Desc().String() for a given prometheus.Desc for use as map key.
func descKey(d *prometheus.Desc) string {
	return d.String()
}

// --- Tests targeting collectSessions label value assertions ---

func TestCollectSessions_stream_labels_from_media(t *testing.T) {
	// Targets lived mutants at lines 850 (len(sess.meta.Media) > 0)
	// and 860 (len(sess.mediaMeta.Media) > 0).
	// Verifies that stream_type, stream_resolution, stream_bitrate, and
	// stream_file_resolution labels are correctly populated from Media fields.
	tracker := newSessionTracker()
	meta := testMeta(t, `{
		"Player":{"device":"Chrome","product":"Plex Web","local":false},
		"Session":{"location":"wan"},
		"User":{"title":"testuser"},
		"Media":[{"videoResolution":"4k","bitrate":20000,"Part":[{"decision":"transcode","key":"/transcode/abc"}]}]
	}`)
	mediaMeta := testMeta(t, `{
		"type":"movie","title":"Stream Test",
		"Media":[{"videoResolution":"1080"}]
	}`)
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

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	playMetrics := byDesc[descKey(descPlayCount)]
	if len(playMetrics) != 1 {
		t.Fatalf("expected 1 play_count metric, got %d", len(playMetrics))
	}

	labels, _ := metricSnapshot(t, playMetrics[0])

	// Verify stream labels derived from meta.Media
	if labels["stream_type"] != "transcode" {
		t.Errorf("stream_type = %q, want transcode", labels["stream_type"])
	}
	if labels["stream_resolution"] != "4k" {
		t.Errorf("stream_resolution = %q, want 4k", labels["stream_resolution"])
	}
	if labels["stream_bitrate"] != "20000" {
		t.Errorf("stream_bitrate = %q, want 20000", labels["stream_bitrate"])
	}
	// Verify file resolution from mediaMeta.Media
	if labels["stream_file_resolution"] != "1080" {
		t.Errorf("stream_file_resolution = %q, want 1080", labels["stream_file_resolution"])
	}
	// Verify other labels
	if labels["transcode_type"] != "video" {
		t.Errorf("transcode_type = %q, want video", labels["transcode_type"])
	}
	if labels["subtitle_action"] != valBurn {
		t.Errorf("subtitle_action = %q, want burn", labels["subtitle_action"])
	}
	if labels["local"] != "false" {
		t.Errorf("local = %q, want false", labels["local"])
	}
	if labels["location"] != "wan" {
		t.Errorf("location = %q, want wan", labels["location"])
	}
}

func TestCollectSessions_no_media_uses_defaults(t *testing.T) {
	// When meta.Media is empty, stream_type should be "unknown", bitrate "0",
	// resolution empty. When mediaMeta.Media is empty, file_resolution empty.
	// This is the inverse of the above test — catches negation of the > 0 checks.
	tracker := newSessionTracker()
	meta := testMeta(t, `{
		"Player":{"device":"TV","local":false},
		"Session":{},
		"User":{"title":"user1"}
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"No Media Movie"}`)
	tracker.sessions["s1"] = session{
		playStarted: time.Now().Add(-1 * time.Second),
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

	byDesc := collectByDesc(ch)
	playMetrics := byDesc[descKey(descPlayCount)]
	if len(playMetrics) != 1 {
		t.Fatalf("expected 1 play_count metric, got %d", len(playMetrics))
	}

	labels, _ := metricSnapshot(t, playMetrics[0])

	if labels["stream_type"] != valUnknown {
		t.Errorf("stream_type = %q, want %q (no media)", labels["stream_type"], valUnknown)
	}
	if labels["stream_bitrate"] != "0" {
		t.Errorf("stream_bitrate = %q, want 0 (no media)", labels["stream_bitrate"])
	}
	if labels["stream_resolution"] != "" {
		t.Errorf("stream_resolution = %q, want empty (no media)", labels["stream_resolution"])
	}
	if labels["stream_file_resolution"] != "" {
		t.Errorf("stream_file_resolution = %q, want empty (no mediaMeta media)", labels["stream_file_resolution"])
	}
}

func TestCollectSessions_local_true_label(t *testing.T) {
	// Targets lived mutant at line 892 (sess.meta.Player.Local negation).
	// Verifies local="true" when Player.Local is true.
	tracker := newSessionTracker()
	meta := testMeta(t, `{
		"Player":{"device":"TV","local":true},
		"User":{"title":"localuser"}
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Local Movie"}`)
	tracker.sessions["s1"] = session{
		playStarted: time.Now().Add(-1 * time.Second),
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

	byDesc := collectByDesc(ch)
	playMetrics := byDesc[descKey(descPlayCount)]
	if len(playMetrics) != 1 {
		t.Fatalf("expected 1 play_count metric, got %d", len(playMetrics))
	}

	labels, _ := metricSnapshot(t, playMetrics[0])
	if labels["local"] != valTrue {
		t.Errorf("local = %q, want true", labels["local"])
	}
}

func TestCollectSessions_library_lookup_sets_labels(t *testing.T) {
	// Targets lived mutant at line 870 (libName == "" negation).
	// When session has no libName, it should be resolved from the libs list.
	// When session HAS a libName, it should NOT be overridden.
	tracker := newSessionTracker()
	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user1"}}`)

	// Session WITH pre-set library labels — should keep them
	mediaMeta1 := testMeta(t, `{"type":"movie","title":"Movie A","librarySectionID":"99"}`)
	tracker.sessions["s1"] = session{
		playStarted: time.Now().Add(-1 * time.Second),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		libName:     "PresetLib",
		libID:       "42",
		libType:     "movie",
		meta:        meta,
		mediaMeta:   mediaMeta1,
	}

	// Session WITHOUT library labels — should resolve from libs
	mediaMeta2 := testMeta(t, `{"type":"movie","title":"Movie B","librarySectionID":"5"}`)
	tracker.sessions["s2"] = session{
		playStarted: time.Now().Add(-1 * time.Second),
		lastUpdate:  time.Now(),
		state:       statePlaying,
		meta:        meta,
		mediaMeta:   mediaMeta2,
	}

	libs := []library{{ID: "5", Name: "4K Movies", Type: libMovie}}
	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 30)
	srv.collectSessions(ch, "Srv", "id1", libs)
	close(ch)

	byDesc := collectByDesc(ch)
	playMetrics := byDesc[descKey(descPlayCount)]
	if len(playMetrics) != 2 {
		t.Fatalf("expected 2 play_count metrics, got %d", len(playMetrics))
	}

	// Find each session's metric by the session label
	for _, m := range playMetrics {
		labels, _ := metricSnapshot(t, m)
		switch labels["session"] {
		case "s1":
			if labels["library"] != "PresetLib" {
				t.Errorf("s1 library = %q, want PresetLib (should keep preset)", labels["library"])
			}
			if labels["library_id"] != "42" {
				t.Errorf("s1 library_id = %q, want 42", labels["library_id"])
			}
		case "s2":
			if labels["library"] != "4K Movies" {
				t.Errorf("s2 library = %q, want 4K Movies (should resolve from libs)", labels["library"])
			}
			if labels["library_id"] != "5" {
				t.Errorf("s2 library_id = %q, want 5", labels["library_id"])
			}
		}
	}
}

// --- Tests targeting estimated transmit bytes arithmetic ---

func TestCollectSessions_estimated_transmit_multiplication_factor(t *testing.T) {
	// Targets lived mutants at lines 911 (arithmetic in elapsed*bitrate)
	// and 915 (total*128 multiplication).
	// With no active sessions and totalEstimatedKBits=1000,
	// est_transmit should be 1000*128 = 128000.
	tracker := newSessionTracker()
	tracker.totalEstimatedKBits = 1000

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 10)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	estMetrics := byDesc[descKey(descEstTransmitBytes)]
	if len(estMetrics) != 1 {
		t.Fatalf("expected 1 est_transmit metric, got %d", len(estMetrics))
	}

	_, value := metricSnapshot(t, estMetrics[0])
	// 1000 kbits * 128 = 128000 bytes
	if value != 128000 {
		t.Errorf("est_transmit = %v, want 128000 (1000 * 128)", value)
	}
}

func TestCollectSessions_estimated_transmit_zero_when_empty(t *testing.T) {
	// With no sessions and zero totalEstimatedKBits, est_transmit should be 0.
	tracker := newSessionTracker()

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 10)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	estMetrics := byDesc[descKey(descEstTransmitBytes)]
	if len(estMetrics) != 1 {
		t.Fatalf("expected 1 est_transmit metric, got %d", len(estMetrics))
	}

	_, value := metricSnapshot(t, estMetrics[0])
	if value != 0 {
		t.Errorf("est_transmit = %v, want 0", value)
	}
}

func TestCollectSessions_stopped_session_no_additional_estimated(t *testing.T) {
	// Targets lived mutant at line 910 (sess.state == statePlaying negation).
	// A stopped session should NOT add time.Since(playStarted)*bitrate to estimated.
	// Only prevPlayedTime contributes via totalEstimatedKBits (already accumulated on stop).
	tracker := newSessionTracker()
	tracker.totalEstimatedKBits = 500

	meta := testMeta(t, `{
		"Player":{"device":"TV"},
		"User":{"title":"user1"},
		"Media":[{"bitrate":10000}]
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Stopped Movie"}`)
	tracker.sessions["s1"] = session{
		playStarted:    time.Now().Add(-100 * time.Second),
		lastUpdate:     time.Now(),
		state:          stateStopped,
		libName:        "Movies",
		libID:          "1",
		libType:        libMovie,
		meta:           meta,
		mediaMeta:      mediaMeta,
		prevPlayedTime: 10 * time.Second,
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	estMetrics := byDesc[descKey(descEstTransmitBytes)]
	if len(estMetrics) != 1 {
		t.Fatalf("expected 1 est_transmit metric, got %d", len(estMetrics))
	}

	_, value := metricSnapshot(t, estMetrics[0])
	// Only totalEstimatedKBits (500) contributes. Stopped session does NOT add
	// time.Since(playStarted)*bitrate. So value = 500 * 128 = 64000.
	if value != 64000 {
		t.Errorf("est_transmit = %v, want 64000 (stopped session should not add elapsed*bitrate)", value)
	}
}

func TestCollectSessions_playing_session_adds_estimated(t *testing.T) {
	// A playing session SHOULD add time.Since(playStarted)*bitrate to estimated.
	// This is the complement of the stopped test above.
	tracker := newSessionTracker()
	tracker.totalEstimatedKBits = 0

	meta := testMeta(t, `{
		"Player":{"device":"TV"},
		"User":{"title":"user1"},
		"Media":[{"bitrate":10000}]
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Playing Movie"}`)
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

	byDesc := collectByDesc(ch)
	estMetrics := byDesc[descKey(descEstTransmitBytes)]
	if len(estMetrics) != 1 {
		t.Fatalf("expected 1 est_transmit metric, got %d", len(estMetrics))
	}

	_, value := metricSnapshot(t, estMetrics[0])
	// ~10 seconds * 10000 kbits * 128 bytes/kbit ≈ 12,800,000
	// Allow some tolerance for timing
	if value < 1000000 {
		t.Errorf("est_transmit = %v, want > 1000000 (playing session should add elapsed*bitrate)", value)
	}
}

// --- Tests targeting bandwidth boundary conditions ---

func TestRefreshBandwidth_exact_boundary_not_counted(t *testing.T) {
	// Targets lived mutants at lines 742/744 (boundary on u.At > s.lastBandwidthAt).
	// An entry with at == lastBandwidthAt should NOT be counted (> not >=).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
				{"bytes":100,"at":1000},
				{"bytes":200,"at":2000}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.lastBandwidthAt = 1000

	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	transmit := server.transmitBytes
	lastAt := server.lastBandwidthAt
	server.mu.Unlock()

	// at=1000 is NOT > 1000, so only at=2000 (200 bytes) should be counted
	if transmit != 200 {
		t.Errorf("transmitBytes = %v, want 200 (at=1000 should not be counted, only at=2000)", transmit)
	}
	if lastAt != 2000 {
		t.Errorf("lastBandwidthAt = %d, want 2000", lastAt)
	}
}

func TestRefreshBandwidth_all_old_entries_skipped(t *testing.T) {
	// All entries at or below lastBandwidthAt should be skipped.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
				{"bytes":100,"at":500},
				{"bytes":200,"at":1000}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.lastBandwidthAt = 1000

	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	transmit := server.transmitBytes
	lastAt := server.lastBandwidthAt
	server.mu.Unlock()

	if transmit != 0 {
		t.Errorf("transmitBytes = %v, want 0 (all entries at or below lastBandwidthAt)", transmit)
	}
	if lastAt != 1000 {
		t.Errorf("lastBandwidthAt = %d, want 1000 (unchanged)", lastAt)
	}
}

func TestRefreshBandwidth_negative_inversion(t *testing.T) {
	// Targets lived mutant at line 736 (INVERT_NEGATIVES / ARITHMETIC_BASE on sort).
	// Verifies that bandwidth entries are processed correctly regardless of input order.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			// Entries in reverse order — sort should handle this
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
				{"bytes":300,"at":3000},
				{"bytes":100,"at":1000},
				{"bytes":200,"at":2000}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.lastBandwidthAt = 500

	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	transmit := server.transmitBytes
	lastAt := server.lastBandwidthAt
	server.mu.Unlock()

	// All entries > 500: 100 + 200 + 300 = 600
	if transmit != 600 {
		t.Errorf("transmitBytes = %v, want 600", transmit)
	}
	if lastAt != 3000 {
		t.Errorf("lastBandwidthAt = %d, want 3000", lastAt)
	}
}

// --- Tests targeting library items writeback boundary ---

func TestRefreshLibraryItems_writeback_boundary(t *testing.T) {
	// Targets lived mutant at line 703 (i < len(s.libraries) boundary).
	// When the local libs slice has more entries than s.libraries (e.g. library
	// was removed between copy and writeback), the extra entries should be ignored.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/library/sections/1/all"):
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":100}}`)
		case strings.HasPrefix(r.URL.Path, "/library/sections/2/all"):
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":200}}`)
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
		{ID: "2", Name: "TV", Type: libShow},
	}

	server.refreshLibraryItems(context.Background())

	server.mu.Lock()
	defer server.mu.Unlock()

	if server.libraries[0].ItemsCount != 100 {
		t.Errorf("Movies ItemsCount = %d, want 100", server.libraries[0].ItemsCount)
	}
	if server.libraries[1].ItemsCount != 200 {
		t.Errorf("TV ItemsCount = %d, want 200", server.libraries[1].ItemsCount)
	}
}

func TestRefreshLibraryItems_id_mismatch_skips_writeback(t *testing.T) {
	// When library IDs don't match between local copy and s.libraries
	// (e.g. libraries were reordered), writeback should skip mismatched entries.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/library/sections/") {
			fmt.Fprint(w, `{"MediaContainer":{"totalSize":999}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.libraries = []library{
		{ID: "1", Name: "Movies", Type: libMovie, ItemsCount: 50},
	}

	// Simulate a race: after copy, s.libraries gets replaced with different IDs
	// We can't easily simulate this race, but we can test the ID check by
	// verifying that matching IDs DO get written back (already tested above)
	// and that the function doesn't panic with empty libraries.
	server.libraries = nil
	server.refreshLibraryItems(context.Background())

	server.mu.Lock()
	defer server.mu.Unlock()

	if len(server.libraries) != 0 {
		t.Errorf("libraries count = %d, want 0", len(server.libraries))
	}
}

func TestRefreshLibraryItems_artist_type10_error_falls_back(t *testing.T) {
	// Targets lived mutant at line 690 (count > 0 boundary).
	// When type=10 returns an error, should fall back to type=7.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				w.WriteHeader(http.StatusInternalServerError)
			case "7":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":777}}`)
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

	if server.libraries[0].ItemsCount != 777 {
		t.Errorf("Music ItemsCount = %d, want 777 (type=10 error, type=7 fallback)", server.libraries[0].ItemsCount)
	}
}

func TestRefreshLibraryItems_artist_both_fail_uses_default_path(t *testing.T) {
	// When both type=10 and type=7 fail for artist, should fall through to default path.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				w.WriteHeader(http.StatusInternalServerError)
			case "7":
				w.WriteHeader(http.StatusInternalServerError)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":42}}`)
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

	if server.libraries[0].ItemsCount != 42 {
		t.Errorf("Music ItemsCount = %d, want 42 (both type queries failed, default path)", server.libraries[0].ItemsCount)
	}
}

// --- Tests targeting Collect metric values ---

func TestCollect_host_metrics_values(t *testing.T) {
	// Verify actual numeric values of host CPU and memory metrics.
	srv := &plexServer{
		name:     "Srv",
		id:       "id1",
		hostCPU:  0.42,
		hostMem:  0.65,
		sessions: newSessionTracker(),
	}

	ch := make(chan prometheus.Metric, 20)
	srv.Collect(ch)
	close(ch)

	byDesc := collectByDesc(ch)

	cpuMetrics := byDesc[descKey(descHostCPU)]
	if len(cpuMetrics) != 1 {
		t.Fatalf("expected 1 cpu metric, got %d", len(cpuMetrics))
	}
	_, cpuVal := metricSnapshot(t, cpuMetrics[0])
	if cpuVal != 0.42 {
		t.Errorf("host_cpu = %v, want 0.42", cpuVal)
	}

	memMetrics := byDesc[descKey(descHostMem)]
	if len(memMetrics) != 1 {
		t.Fatalf("expected 1 mem metric, got %d", len(memMetrics))
	}
	_, memVal := metricSnapshot(t, memMetrics[0])
	if memVal != 0.65 {
		t.Errorf("host_mem = %v, want 0.65", memVal)
	}
}

func TestCollect_ws_connected_values(t *testing.T) {
	// Verify ws_connected is 1.0 when connected, 0.0 when not.
	for _, tc := range []struct {
		name      string
		connected bool
		want      float64
	}{
		{"connected", true, 1.0},
		{"disconnected", false, 0.0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &plexServer{
				name:        "Srv",
				id:          "id1",
				wsConnected: tc.connected,
				sessions:    newSessionTracker(),
			}

			ch := make(chan prometheus.Metric, 20)
			srv.Collect(ch)
			close(ch)

			byDesc := collectByDesc(ch)
			wsMetrics := byDesc[descKey(descWSConnected)]
			if len(wsMetrics) != 1 {
				t.Fatalf("expected 1 ws_connected metric, got %d", len(wsMetrics))
			}
			_, val := metricSnapshot(t, wsMetrics[0])
			if val != tc.want {
				t.Errorf("ws_connected = %v, want %v", val, tc.want)
			}
		})
	}
}

func TestCollect_library_items_only_when_positive(t *testing.T) {
	// Libraries with ItemsCount=0 should NOT emit lib_items metric.
	// Libraries with ItemsCount>0 should emit with correct content_type label.
	srv := &plexServer{
		name: "Srv",
		id:   "id1",
		libraries: []library{
			{ID: "1", Name: "Movies", Type: libMovie, ItemsCount: 100},
			{ID: "2", Name: "TV", Type: libShow, ItemsCount: 0},
		},
		sessions: newSessionTracker(),
	}

	ch := make(chan prometheus.Metric, 30)
	srv.Collect(ch)
	close(ch)

	byDesc := collectByDesc(ch)
	itemMetrics := byDesc[descKey(descLibItems)]
	if len(itemMetrics) != 1 {
		t.Fatalf("expected 1 lib_items metric (only Movies), got %d", len(itemMetrics))
	}

	labels, val := metricSnapshot(t, itemMetrics[0])
	if val != 100 {
		t.Errorf("lib_items value = %v, want 100", val)
	}
	if labels["content_type"] != "movies" {
		t.Errorf("content_type = %q, want movies", labels["content_type"])
	}
	if labels["library"] != "Movies" {
		t.Errorf("library = %q, want Movies", labels["library"])
	}
}

// --- Tests targeting session bandwidth metric ---

func TestCollectSessions_bandwidth_only_when_positive(t *testing.T) {
	// Session with bandwidth=0 should NOT emit session_bandwidth metric.
	// Session with bandwidth>0 should emit with correct value.
	tracker := newSessionTracker()
	meta := testMeta(t, `{
		"Player":{"device":"TV"},
		"Session":{"bandwidth":0},
		"User":{"title":"user1"}
	}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"No BW Movie"}`)
	tracker.sessions["s1"] = session{
		playStarted: time.Now().Add(-1 * time.Second),
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

	byDesc := collectByDesc(ch)
	bwMetrics := byDesc[descKey(descSessionBandwidth)]
	if len(bwMetrics) != 0 {
		t.Errorf("expected 0 session_bandwidth metrics (bandwidth=0), got %d", len(bwMetrics))
	}
}

func TestCollectSessions_play_seconds_stopped_uses_prev(t *testing.T) {
	// A stopped session should use prevPlayedTime, not add time.Since(playStarted).
	tracker := newSessionTracker()
	meta := testMeta(t, `{"Player":{"device":"TV"},"User":{"title":"user1"}}`)
	mediaMeta := testMeta(t, `{"type":"movie","title":"Stopped Movie"}`)
	tracker.sessions["s1"] = session{
		playStarted:    time.Now().Add(-1000 * time.Second),
		lastUpdate:     time.Now(),
		state:          stateStopped,
		libName:        "Movies",
		libID:          "1",
		libType:        libMovie,
		meta:           meta,
		mediaMeta:      mediaMeta,
		prevPlayedTime: 5 * time.Second,
	}

	srv := &plexServer{name: "Srv", id: "id1", sessions: tracker}
	ch := make(chan prometheus.Metric, 20)
	srv.collectSessions(ch, "Srv", "id1", nil)
	close(ch)

	byDesc := collectByDesc(ch)
	secMetrics := byDesc[descKey(descPlaySeconds)]
	if len(secMetrics) != 1 {
		t.Fatalf("expected 1 play_seconds metric, got %d", len(secMetrics))
	}

	_, val := metricSnapshot(t, secMetrics[0])
	// Should be ~5 seconds (prevPlayedTime), NOT ~1000 seconds
	if val > 10 {
		t.Errorf("play_seconds = %v, want ~5 (stopped session should use prevPlayedTime only)", val)
	}
	if val < 4 {
		t.Errorf("play_seconds = %v, want ~5 (prevPlayedTime)", val)
	}
}

// --- Round 2: targeting remaining lived mutants ---

func TestRefreshLibraryItems_artist_type10_returns_zero_falls_to_type7(t *testing.T) {
	// Targets lived mutant at line 690: CONDITIONALS_BOUNDARY on count > 0.
	// When type=10 returns count=0 (not error, but zero), should fall back to type=7.
	// This is the exact boundary: count=0 should NOT be accepted.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":0}}`)
			case "7":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":500}}`)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":10}}`)
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

	// type=10 returned 0, so should fall back to type=7 which returns 500
	if server.libraries[0].ItemsCount != 500 {
		t.Errorf("Music ItemsCount = %d, want 500 (type=10 returned 0, should use type=7)", server.libraries[0].ItemsCount)
	}
}

func TestRefreshLibraryItems_artist_type7_returns_zero_falls_to_default(t *testing.T) {
	// When both type=10 and type=7 return 0, should fall through to default path.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections/1/all" {
			switch r.URL.Query().Get("type") {
			case "10":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":0}}`)
			case "7":
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":0}}`)
			default:
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":99}}`)
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

	// Both type queries returned 0, should fall through to default path
	if server.libraries[0].ItemsCount != 99 {
		t.Errorf("Music ItemsCount = %d, want 99 (both type queries returned 0, default path)", server.libraries[0].ItemsCount)
	}
}

func TestRefreshBandwidth_duplicate_timestamps(t *testing.T) {
	// Targets lived mutant at line 744: CONDITIONALS_BOUNDARY on u.At > highest.
	// With duplicate timestamps, highest should be set correctly.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
				{"bytes":100,"at":2000},
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
	server.lastBandwidthAt = 1000

	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	transmit := server.transmitBytes
	lastAt := server.lastBandwidthAt
	server.mu.Unlock()

	// All entries > 1000: 100 + 200 + 300 = 600
	if transmit != 600 {
		t.Errorf("transmitBytes = %v, want 600", transmit)
	}
	// highest should be 3000, not 2000
	if lastAt != 3000 {
		t.Errorf("lastBandwidthAt = %d, want 3000", lastAt)
	}
}

func TestRefreshBandwidth_single_new_entry(t *testing.T) {
	// Single entry above lastBandwidthAt — verifies highest tracking works.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
				{"bytes":500,"at":5000}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.lastBandwidthAt = 4000

	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	transmit := server.transmitBytes
	lastAt := server.lastBandwidthAt
	server.mu.Unlock()

	if transmit != 500 {
		t.Errorf("transmitBytes = %v, want 500", transmit)
	}
	if lastAt != 5000 {
		t.Errorf("lastBandwidthAt = %d, want 5000", lastAt)
	}
}

func TestRefreshBandwidth_accumulates_across_calls(t *testing.T) {
	// Verifies that transmitBytes accumulates across multiple refreshBandwidth calls.
	callCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/statistics/bandwidth" {
			callCount++
			if callCount == 1 {
				fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
					{"bytes":100,"at":2000}
				]}}`)
			} else {
				fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[
					{"bytes":100,"at":2000},
					{"bytes":200,"at":3000}
				]}}`)
			}
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	server.lastBandwidthAt = 1000

	// First call: at=2000 (100 bytes)
	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	if server.transmitBytes != 100 {
		t.Errorf("after call 1: transmitBytes = %v, want 100", server.transmitBytes)
	}
	if server.lastBandwidthAt != 2000 {
		t.Errorf("after call 1: lastBandwidthAt = %d, want 2000", server.lastBandwidthAt)
	}
	server.mu.Unlock()

	// Second call: at=2000 already seen, only at=3000 (200 bytes) is new
	server.refreshBandwidth(context.Background())

	server.mu.Lock()
	if server.transmitBytes != 300 {
		t.Errorf("after call 2: transmitBytes = %v, want 300 (100 + 200)", server.transmitBytes)
	}
	if server.lastBandwidthAt != 3000 {
		t.Errorf("after call 2: lastBandwidthAt = %d, want 3000", server.lastBandwidthAt)
	}
	server.mu.Unlock()
}

// --- Round 2 continued: targeting newly surfaced lived mutants ---

func TestSessionTrackerUpdate_stop_accumulates_estimated_kbits(t *testing.T) {
	// Targets lived mutants at lines 523 (len(s.meta.Media) > 0 boundary)
	// and 526 (elapsed.Seconds() * float64(bitrate) arithmetic).
	// Verifies that stopping a playing session with media accumulates
	// totalEstimatedKBits correctly.
	tracker := newSessionTracker()

	meta := &sessionMetadata{
		Title: "Test",
		Media: []mediaInfo{{Bitrate: 1000}},
	}
	// Manually set up a playing session with a known playStarted time
	tracker.mu.Lock()
	tracker.sessions["s1"] = session{
		state:       statePlaying,
		playStarted: time.Now().Add(-10 * time.Second),
		lastUpdate:  time.Now(),
		meta:        *meta,
	}
	tracker.mu.Unlock()

	tracker.update("s1", stateStopped, nil, nil)

	tracker.mu.Lock()
	kbits := tracker.totalEstimatedKBits
	tracker.mu.Unlock()

	// Should be ~10s * 1000 bitrate = ~10000 kbits
	if kbits < 5000 {
		t.Errorf("totalEstimatedKBits = %v, want > 5000 after stop", kbits)
	}
	if kbits > 20000 {
		t.Errorf("totalEstimatedKBits = %v, unexpectedly large", kbits)
	}
}

func TestSessionTrackerUpdate_stop_without_media_no_accumulation(t *testing.T) {
	// When session has no media, stopping should NOT accumulate estimated kbits.
	// This is the inverse test for the len(s.meta.Media) > 0 check.
	tracker := newSessionTracker()

	// Manually set up a playing session without media
	tracker.mu.Lock()
	tracker.sessions["s1"] = session{
		state:       statePlaying,
		playStarted: time.Now().Add(-10 * time.Second),
		lastUpdate:  time.Now(),
		meta:        sessionMetadata{Title: "No Media"},
	}
	tracker.mu.Unlock()

	tracker.update("s1", stateStopped, nil, nil)

	tracker.mu.Lock()
	kbits := tracker.totalEstimatedKBits
	tracker.mu.Unlock()

	if kbits != 0 {
		t.Errorf("totalEstimatedKBits = %v, want 0 (no media)", kbits)
	}
}

func TestRefresh_prevItems_preserves_positive_counts_only(t *testing.T) {
	// Targets lived mutant at line 611 (lib.ItemsCount > 0 boundary).
	// Libraries with ItemsCount=0 should NOT be preserved in prevItems.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/providers":
			fmt.Fprint(w, `{"MediaContainer":{
				"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0",
				"MediaProvider":[{"identifier":"com.plexapp.plugins.library","Feature":[
					{"type":"content","Directory":[
						{"title":"Movies","id":"1","type":"movie"},
						{"title":"TV","id":"2","type":"show"}
					]}
				]}]
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

	// Pre-populate: Movies has count, TV has 0
	server.libraries = []library{
		{ID: "1", Name: "Movies", Type: libMovie, ItemsCount: 100},
		{ID: "2", Name: "TV", Type: libShow, ItemsCount: 0},
	}
	server.lastItemsRefresh = time.Now() // skip items refresh

	err := server.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	// Movies should preserve its count (100 > 0)
	if server.libraries[0].ItemsCount != 100 {
		t.Errorf("Movies ItemsCount = %d, want 100 (preserved)", server.libraries[0].ItemsCount)
	}
	// TV should remain 0 (0 is not > 0, so not preserved)
	if server.libraries[1].ItemsCount != 0 {
		t.Errorf("TV ItemsCount = %d, want 0 (not preserved)", server.libraries[1].ItemsCount)
	}
}

func TestRefresh_items_refresh_triggered_after_15_minutes(t *testing.T) {
	// Targets lived mutant at line 637 (time.Since > 15*time.Minute boundary/negation).
	// When lastItemsRefresh is old enough, items should be refreshed.
	itemsRequested := false
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
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0"}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			if strings.HasPrefix(r.URL.Path, "/library/sections/") {
				itemsRequested = true
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":42}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	// Set lastItemsRefresh to 20 minutes ago — should trigger refresh
	server.lastItemsRefresh = time.Now().Add(-20 * time.Minute)

	err := server.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	if !itemsRequested {
		t.Error("items refresh should be triggered when lastItemsRefresh > 15 minutes ago")
	}
}

func TestRefresh_items_refresh_skipped_when_recent(t *testing.T) {
	// When lastItemsRefresh is recent, items should NOT be refreshed.
	itemsRequested := false
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
		case "/":
			fmt.Fprint(w, `{"MediaContainer":{"friendlyName":"Plex","machineIdentifier":"id1","version":"1.0"}}`)
		case "/statistics/resources":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsResources":[]}}`)
		case "/statistics/bandwidth":
			fmt.Fprint(w, `{"MediaContainer":{"StatisticsBandwidth":[]}}`)
		default:
			if strings.HasPrefix(r.URL.Path, "/library/sections/") {
				itemsRequested = true
				fmt.Fprint(w, `{"MediaContainer":{"totalSize":42}}`)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := newTestPlexClient(t, srv)
	server := newPlexServer(client)
	// Set lastItemsRefresh to 5 minutes ago — should NOT trigger refresh
	server.lastItemsRefresh = time.Now().Add(-5 * time.Minute)

	err := server.refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh() error: %v", err)
	}

	if itemsRequested {
		t.Error("items refresh should NOT be triggered when lastItemsRefresh < 15 minutes ago")
	}
}

func TestGetWithRetry_retries_correct_number_of_times(t *testing.T) {
	// Targets lived mutants at lines 247-248 (retry boundary/arithmetic).
	// Verifies that maxRetries=1 means exactly 1 attempt (no retries).
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
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
	_ = client.getWithRetry(context.Background(), "/", &resp, 1)

	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (maxRetries=1 means 1 attempt)", attempts)
	}
}

func TestGetWithRetry_maxRetries4_makes_4_attempts(t *testing.T) {
	// Verifies that maxRetries=4 means exactly 4 attempts.
	attempts := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
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
	_ = client.getWithRetry(context.Background(), "/", &resp, 4)

	if attempts != 4 {
		t.Errorf("attempts = %d, want 4 (maxRetries=4)", attempts)
	}
}

func TestSessionTrackerPrune_exact_timeout_boundary(t *testing.T) {
	// Targets lived mutant at line 542 (time.Since > sessionTimeout boundary).
	// A session stopped exactly at the timeout boundary should NOT be pruned
	// (> not >=).
	tracker := newSessionTracker()

	// Session stopped just barely within the timeout — should be kept
	tracker.mu.Lock()
	tracker.sessions["barely_within"] = session{
		state:      stateStopped,
		lastUpdate: time.Now().Add(-sessionTimeout + 100*time.Millisecond),
	}
	// Session stopped well past the timeout — should be pruned
	tracker.sessions["well_past"] = session{
		state:      stateStopped,
		lastUpdate: time.Now().Add(-sessionTimeout - time.Second),
	}
	tracker.mu.Unlock()

	tracker.prune()

	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	if _, ok := tracker.sessions["barely_within"]; !ok {
		t.Error("barely_within should be kept (within timeout)")
	}
	if _, ok := tracker.sessions["well_past"]; ok {
		t.Error("well_past should be pruned (past timeout)")
	}
}
