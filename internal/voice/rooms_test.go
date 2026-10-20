package voice

import (
	"testing"

	"github.com/google/uuid"
	"github.com/livekit/protocol/livekit"
)

func TestToParticipant(t *testing.T) {
	userID := uuid.New()
	info := &livekit.ParticipantInfo{
		Identity: userID.String(),
		Name:     "Anna",
		Attributes: map[string]string{
			"deafened": "true",
		},
		Tracks: []*livekit.TrackInfo{
			{Source: livekit.TrackSource_MICROPHONE, Muted: true},
			{Source: livekit.TrackSource_SCREEN_SHARE, Type: livekit.TrackType_VIDEO, Muted: false},
		},
	}

	participant, ok := toParticipant(info)
	if !ok {
		t.Fatal("a participant with an account id must be kept")
	}
	if participant.UserID != userID || participant.Name != "Anna" {
		t.Errorf("participant = %+v, want %s / Anna", participant, userID)
	}
	if !participant.Muted {
		t.Error("a muted microphone must be reported as muted")
	}
	if !participant.Deafened {
		t.Error("the deafened attribute must be reported")
	}
	if !participant.Screen {
		t.Error("an unmuted screen track must be reported")
	}
}

func TestToParticipantWithoutMicrophone(t *testing.T) {
	participant, ok := toParticipant(&livekit.ParticipantInfo{
		Identity: uuid.New().String(),
		Tracks:   []*livekit.TrackInfo{{Source: livekit.TrackSource_SCREEN_SHARE, Muted: true}},
	})
	if !ok {
		t.Fatal("participant must be kept")
	}
	if !participant.Muted {
		t.Error("a participant without a microphone track is muted")
	}
	if participant.Screen {
		t.Error("a muted screen track is not a screen share")
	}
}

func TestToParticipantSkipsForeignIdentity(t *testing.T) {
	if _, ok := toParticipant(&livekit.ParticipantInfo{Identity: "guest-123"}); ok {
		t.Error("an identity that is not an account id must be skipped")
	}
}

func TestAPIURL(t *testing.T) {
	tests := map[string]string{
		"ws://localhost:7880":   "http://localhost:7880",
		"wss://live.example":    "https://live.example",
		"http://127.0.0.1:7880": "http://127.0.0.1:7880",
	}
	for raw, want := range tests {
		got, err := apiURL(raw)
		if err != nil {
			t.Errorf("apiURL(%q) error = %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("apiURL(%q) = %q, want %q", raw, got, want)
		}
	}
	if _, err := apiURL("rtmp://example"); err == nil {
		t.Error("an unsupported scheme must be rejected")
	}
}
