package main

import "testing"

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
