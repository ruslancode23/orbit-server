package voice

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
)

// Participant is one person sitting in a voice room, as LiveKit sees them.
type Participant struct {
	UserID   uuid.UUID
	Name     string
	Muted    bool
	Deafened bool
	Screen   bool
}

// Rooms reads and closes LiveKit rooms through its RoomService API. The
// protocol module already carries the generated client; the only thing added
// here is the server-signed JWT LiveKit wants on every call.
type Rooms struct {
	base      string
	apiKey    string
	apiSecret string
}

func NewRooms(livekitURL, apiKey, apiSecret string) (*Rooms, error) {
	base, err := apiURL(livekitURL)
	if err != nil {
		return nil, err
	}
	return &Rooms{base: base, apiKey: apiKey, apiSecret: apiSecret}, nil
}

// clientFor signs a token bound to one room: LiveKit's RoomService checks that
// the caller may join the room it asks about, even for an administrative read.
func (r *Rooms) clientFor(room string) livekit.RoomService {
	return livekit.NewRoomServiceProtobufClient(r.base,
		&signedClient{key: r.apiKey, secret: r.apiSecret, room: room})
}

func (r *Rooms) List(ctx context.Context, room string) ([]Participant, error) {
	response, err := r.clientFor(room).ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		return nil, fmt.Errorf("list participants: %w", err)
	}
	out := make([]Participant, 0, len(response.Participants))
	for _, info := range response.Participants {
		participant, ok := toParticipant(info)
		if ok {
			out = append(out, participant)
		}
	}
	return out, nil
}

func (r *Rooms) Delete(ctx context.Context, room string) error {
	if _, err := r.clientFor(room).DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: room}); err != nil {
		return fmt.Errorf("delete room: %w", err)
	}
	return nil
}

// toParticipant maps LiveKit's view onto what the sidebar shows. An identity
// that is not an account id belongs to the dev spike and is ignored.
func toParticipant(info *livekit.ParticipantInfo) (Participant, bool) {
	userID, err := uuid.Parse(info.GetIdentity())
	if err != nil {
		return Participant{}, false
	}
	muted := true
	screen := false
	for _, track := range info.GetTracks() {
		switch track.GetSource() {
		case livekit.TrackSource_MICROPHONE:
			muted = track.GetMuted()
		case livekit.TrackSource_SCREEN_SHARE:
			screen = !track.GetMuted()
		}
	}
	name := info.GetName()
	if name == "" {
		name = info.GetIdentity()
	}
	return Participant{
		UserID:   userID,
		Name:     name,
		Muted:    muted,
		Deafened: info.GetAttributes()["deafened"] == "true",
		Screen:   screen,
	}, true
}

// apiURL turns the client WebSocket URL into the HTTP endpoint of the same
// LiveKit instance: RoomService lives on the same host and port.
func apiURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse LiveKit URL: %w", err)
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("LiveKit URL scheme must be ws, wss, http or https, got %q", parsed.Scheme)
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// signedClient adds a short-lived server token to every RoomService request.
type signedClient struct {
	key    string
	secret string
	room   string
}

func (c *signedClient) Do(request *http.Request) (*http.Response, error) {
	token, err := auth.NewAccessToken(c.key, c.secret).
		SetIdentity("orbit-server").
		SetValidFor(time.Minute).
		SetVideoGrant(&auth.VideoGrant{RoomAdmin: true, RoomJoin: true, Room: c.room}).
		ToJWT()
	if err != nil {
		return nil, fmt.Errorf("sign LiveKit admin token: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request) //nolint:gosec // the URL is operator configuration, not request input
	if err != nil {
		return nil, fmt.Errorf("call LiveKit RoomService: %w", err)
	}
	return response, nil
}
