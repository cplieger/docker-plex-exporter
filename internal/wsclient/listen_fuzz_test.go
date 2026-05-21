package wsclient

import (
	"encoding/json"
	"testing"

	"plex-exporter/internal/plexapi"
)

func FuzzWSNotificationUnmarshal(f *testing.F) {
	for _, seed := range []string{
		`{"NotificationContainer":{"type":"playing","PlaySessionStateNotification":[{"sessionKey":"1","state":"playing","viewOffset":1000}]}}`,
		`{"NotificationContainer":{"type":"transcodeSession.update","TranscodeSession":[{"key":"/transcode/sessions/abc","progress":50}]}}`,
		`{"NotificationContainer":{"type":"unknown"}}`,
		`{}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		var notif plexapi.WSNotification
		_ = json.Unmarshal(data, &notif)
	})
}
