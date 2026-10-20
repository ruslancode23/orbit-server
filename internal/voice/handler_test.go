package voice

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

var testChannel = uuid.MustParse("11111111-1111-4111-8111-111111111111")

type accessStub struct {
	canSpeak bool
	err      error
}

func (a *accessStub) VoiceAccess(_ context.Context, _, _ uuid.UUID) (bool, error) {
	return a.canSpeak, a.err
}

func newTestHandler(access *accessStub) *Handler {
	return NewHandler(NewTokenIssuer(testKey, testSecret, testURL), access,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestCreateToken(t *testing.T) {
	userID := uuid.New()
	resp, err := newTestHandler(&accessStub{canSpeak: true}).CreateChannelVoiceToken(
		auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: userID, Nickname: "Anna"}}),
		api.CreateChannelVoiceTokenRequestObject{ChannelId: testChannel},
	)
	if err != nil {
		t.Fatalf("CreateChannelVoiceToken() error = %v", err)
	}

	created, ok := resp.(api.CreateChannelVoiceToken201JSONResponse)
	if !ok {
		t.Fatalf("response = %#v, want 201", resp)
	}
	if created.Url != testURL || created.Identity == "" || created.ExpiresAt.IsZero() {
		t.Errorf("incomplete response: %+v", created)
	}
	if created.Identity != userID.String() {
		t.Errorf("identity = %q, want the account id %q", created.Identity, userID)
	}
	if g := verify(t, created.Token); g.Video.Room != testChannel.String() {
		t.Errorf("room = %q, want %q", g.Video.Room, testChannel)
	}
}

func TestCreateTokenWithoutSession(t *testing.T) {
	resp, err := newTestHandler(&accessStub{}).CreateChannelVoiceToken(
		t.Context(), api.CreateChannelVoiceTokenRequestObject{ChannelId: testChannel})
	if err != nil {
		t.Fatalf("CreateChannelVoiceToken() error = %v", err)
	}

	if _, ok := resp.(api.CreateChannelVoiceToken401JSONResponse); !ok {
		t.Fatalf("response = %#v, want 401", resp)
	}
}

func TestCreateTokenForMissingChannel(t *testing.T) {
	resp, err := newTestHandler(&accessStub{err: channels.ErrNotFound}).CreateChannelVoiceToken(
		auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: uuid.New(), Nickname: "Anna"}}),
		api.CreateChannelVoiceTokenRequestObject{ChannelId: testChannel},
	)
	if err != nil {
		t.Fatalf("CreateChannelVoiceToken() error = %v", err)
	}

	if _, ok := resp.(api.CreateChannelVoiceToken404JSONResponse); !ok {
		t.Fatalf("response = %#v, want 404", resp)
	}
}

func TestCreateTokenAccessFailure(t *testing.T) {
	resp, err := newTestHandler(&accessStub{err: errors.New("database is down")}).CreateChannelVoiceToken(
		auth.WithSession(t.Context(), auth.Session{User: auth.User{ID: uuid.New(), Nickname: "Anna"}}),
		api.CreateChannelVoiceTokenRequestObject{ChannelId: testChannel},
	)
	if err != nil {
		t.Fatalf("CreateChannelVoiceToken() error = %v", err)
	}

	internal, ok := resp.(api.CreateChannelVoiceToken500JSONResponse)
	if !ok {
		t.Fatalf("response = %#v, want 500", resp)
	}
	if internal.Error.Code != httpx.CodeInternal {
		t.Errorf("code = %q, want %q", internal.Error.Code, httpx.CodeInternal)
	}
}
