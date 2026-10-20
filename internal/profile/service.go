package profile

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/events"
)

const (
	// debounce is the least time between two user.updated announcements about one
	// account. A settings screen saves a few fields in a row; its audience needs
	// one update, not one per field.
	debounce = 2 * time.Second
	// tick is how often Run sends what has come due.
	tick = 250 * time.Millisecond
)

// Store is the persistence this package needs. It owns the profile columns of
// users; the interface lives with its consumer and store.go implements it.
type Store interface {
	// Profile returns ErrNotFound for an unknown account.
	Profile(ctx context.Context, userID uuid.UUID) (Stored, error)
	// Profiles returns whichever of the ids exist, so a caller never asks per id.
	Profiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Stored, error)
	UpdateProfile(ctx context.Context, userID uuid.UUID, u Update) (Stored, error)
	// SetStatus replaces the status, including its deadline; the empty strings
	// become NULLs.
	SetStatus(ctx context.Context, userID uuid.UUID, emoji, text string, expiresAt *time.Time) (Stored, error)
	ClearStatus(ctx context.Context, userID uuid.UUID) (Stored, error)
	// SetAvatar points the account at a new avatar key and returns the key it
	// pointed at before, so the caller can delete the old objects.
	SetAvatar(ctx context.Context, userID uuid.UUID, key string) (Stored, string, error)
}

// Servers tells which servers an account is on: everyone there may see its
// profile, and every server is an audience of user.updated.
type Servers interface {
	Servers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// Conversations is the conversations module, reached through its own interface:
// this package never reads its tables.
type Conversations interface {
	// SharesConversation reports a common ordinary (direct or group) conversation.
	SharesConversation(ctx context.Context, a, b uuid.UUID) (bool, error)
	// Peers is everyone who shares a conversation with this account: the personal
	// audience of user.updated.
	Peers(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// Friends is the friendship module of FR-1. It is optional: with nil the friend
// path simply does not fire, and no other rule depends on it.
type Friends interface {
	AreFriends(ctx context.Context, a, b uuid.UUID) (bool, error)
	FriendsOf(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
}

// Publisher is the live stream profile changes are announced to. A nil
// publisher is allowed: the API works the same and nobody is told live.
type Publisher interface {
	Publish(event events.Event)
	PublishToUser(userID uuid.UUID, event events.Event)
}

// AvatarStorage keeps the re-encoded avatar objects and signs links to them.
// The object store fits; the interface lives here because that is where it is
// consumed.
type AvatarStorage interface {
	Put(ctx context.Context, key, mimeType string, body io.Reader, size int64) error
	// SignedInlineURL is a link the browser renders rather than downloads.
	SignedInlineURL(ctx context.Context, key, mimeType string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

// Avatars configures avatar uploads and links. The zero value turns uploads
// off: an account with a key then simply shows no avatar.
type Avatars struct {
	Storage  AvatarStorage
	MaxBytes int64
	// LinkTTL is how long an avatarUrl stays valid. S3 caps it at seven days.
	LinkTTL time.Duration
}

// Service holds the rules: validation, what a profile hides, who may read it,
// and when the announcement goes out.
type Service struct {
	store Store
	hub   Publisher
	log   *slog.Logger

	servers       Servers
	conversations Conversations
	friends       Friends
	avatars       Avatars

	now      func() time.Time
	debounce time.Duration
	tick     time.Duration

	mu sync.Mutex
	// due is when to announce an account again; the entry disappears once the
	// announcement has gone out.
	due map[uuid.UUID]time.Time
	// sent is when the last announcement about an account went out, which is what
	// the debounce counts from.
	sent map[uuid.UUID]time.Time
}

func NewService(store Store, hub Publisher, log *slog.Logger) *Service {
	return &Service{
		store:    store,
		hub:      hub,
		log:      log,
		now:      time.Now,
		debounce: debounce,
		tick:     tick,
		due:      map[uuid.UUID]time.Time{},
		sent:     map[uuid.UUID]time.Time{},
	}
}

// SetServers wires in the servers module after both exist. Optional: without it
// nobody hears about a change and the shared-server check does not fire.
func (s *Service) SetServers(servers Servers) { s.servers = servers }

// SetConversations wires in the conversations module after both exist.
func (s *Service) SetConversations(conversations Conversations) { s.conversations = conversations }

// SetFriends wires in the friendships module. Optional: nil means the friend path
// simply does not fire.
func (s *Service) SetFriends(friends Friends) { s.friends = friends }

// SetAvatars wires in signed avatar links. Optional: without it avatarUrl stays
// absent, and PR-2 adds the upload that fills the key.
func (s *Service) SetAvatars(avatars Avatars) { s.avatars = avatars }

// Profile is one account's public profile, expired status already dropped.
func (s *Service) Profile(ctx context.Context, userID uuid.UUID) (Public, error) {
	stored, err := s.store.Profile(ctx, userID)
	if err != nil {
		return Public{}, err
	}
	return s.present(ctx, stored), nil
}

// Profiles is the public profile of every id that exists, in one store call.
// Consumers use it to fill a member list or a page of authors without an N+1.
func (s *Service) Profiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Public, error) {
	if len(ids) == 0 {
		return map[uuid.UUID]Public{}, nil
	}
	stored, err := s.store.Profiles(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]Public, len(stored))
	for id, profile := range stored {
		out[id] = s.present(ctx, profile)
	}
	return out, nil
}

// UserProfiles is Profiles in the API shape. It exists for internal/auth: the
// profile handler reads sessions from auth, so auth cannot import this package
// back and asks in terms of the generated type instead.
func (s *Service) UserProfiles(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]api.UserProfile, error) {
	profiles, err := s.Profiles(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]api.UserProfile, len(profiles))
	for id, profile := range profiles {
		out[id] = APIProfile(profile)
	}
	return out, nil
}

// PublicProfile is one profile as viewer may see it. Your own is always there;
// someone else's needs a common server, a common conversation or a friendship.
// Anything else is ErrNotFound, the same as an unknown id.
func (s *Service) PublicProfile(ctx context.Context, viewerID, targetID uuid.UUID) (Public, error) {
	if targetID == uuid.Nil {
		return Public{}, ErrNotFound
	}
	if viewerID != targetID {
		known, err := s.acquainted(ctx, viewerID, targetID)
		if err != nil {
			return Public{}, err
		}
		if !known {
			return Public{}, ErrNotFound
		}
	}
	return s.Profile(ctx, targetID)
}

// acquainted reports whether the two accounts may see each other's profile.
func (s *Service) acquainted(ctx context.Context, a, b uuid.UUID) (bool, error) {
	if s.friends != nil {
		friends, err := s.friends.AreFriends(ctx, a, b)
		if err != nil {
			return false, fmt.Errorf("check friendship: %w", err)
		}
		if friends {
			return true, nil
		}
	}

	if s.servers != nil {
		mine, err := s.servers.Servers(ctx, a)
		if err != nil {
			return false, fmt.Errorf("list servers of the caller: %w", err)
		}
		theirs, err := s.servers.Servers(ctx, b)
		if err != nil {
			return false, fmt.Errorf("list servers of the target: %w", err)
		}
		seen := make(map[uuid.UUID]bool, len(mine))
		for _, id := range mine {
			seen[id] = true
		}
		for _, id := range theirs {
			if seen[id] {
				return true, nil
			}
		}
	}

	if s.conversations != nil {
		shared, err := s.conversations.SharesConversation(ctx, a, b)
		if err != nil {
			return false, fmt.Errorf("check shared conversation: %w", err)
		}
		return shared, nil
	}
	return false, nil
}

// Update changes the named fields and schedules one announcement. A field not
// named is not touched.
func (s *Service) Update(ctx context.Context, userID uuid.UUID, u Update) (Public, error) {
	if u.empty() {
		return Public{}, ErrNothingToUpdate
	}
	if u.DisplayName != nil {
		name, err := cleanDisplayName(*u.DisplayName)
		if err != nil {
			return Public{}, err
		}
		u.DisplayName = &name
	}
	if u.Bio != nil {
		bio, err := cleanBio(*u.Bio)
		if err != nil {
			return Public{}, err
		}
		u.Bio = &bio
	}
	if u.AccentColor != nil && !u.AccentColor.Valid() {
		return Public{}, ErrInvalidAccent
	}

	stored, err := s.store.UpdateProfile(ctx, userID, u)
	if err != nil {
		return Public{}, err
	}
	s.schedule(userID)
	return s.present(ctx, stored), nil
}

// SetStatus replaces the custom status. The deadline is computed here, on the
// server clock, and never taken from the client.
func (s *Service) SetStatus(ctx context.Context, userID uuid.UUID, update StatusUpdate) (Public, error) {
	cleaned, err := update.cleaned()
	if err != nil {
		return Public{}, err
	}
	expiresAt := cleaned.ExpiresIn.deadline(s.now())
	stored, err := s.store.SetStatus(ctx, userID, cleaned.Emoji, cleaned.Text, expiresAt)
	if err != nil {
		return Public{}, err
	}
	s.schedule(userID)
	return s.present(ctx, stored), nil
}

// ClearStatus removes the custom status. Removing one that is not set is fine.
func (s *Service) ClearStatus(ctx context.Context, userID uuid.UUID) (Public, error) {
	stored, err := s.store.ClearStatus(ctx, userID)
	if err != nil {
		return Public{}, err
	}
	s.schedule(userID)
	return s.present(ctx, stored), nil
}

// Run sends what is due until the server stops. The debounce has no other clock:
// it is measured here, not in a request.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flush(ctx)
		}
	}
}

// schedule asks for an announcement about one account. Nothing is sent now: the
// announcement waits out the debounce, and asking twice never moves a pending
// one later — a burst of saves becomes one event at the end of the window.
func (s *Service) schedule(userID uuid.UUID) {
	now := s.now()
	at := now

	s.mu.Lock()
	defer s.mu.Unlock()

	if previous, ok := s.sent[userID]; ok && previous.Add(s.debounce).After(at) {
		at = previous.Add(s.debounce)
	}
	if pending, ok := s.due[userID]; !ok || at.Before(pending) {
		s.due[userID] = at
	}
}

// flush sends every announcement that has come due. It is the only place that
// publishes, so two changes close in time cannot arrive out of order.
func (s *Service) flush(ctx context.Context) {
	now := s.now()

	s.mu.Lock()
	ready := make([]uuid.UUID, 0, len(s.due))
	for userID, at := range s.due {
		if !at.After(now) {
			ready = append(ready, userID)
			delete(s.due, userID)
		}
	}
	s.mu.Unlock()

	for _, userID := range ready {
		s.announce(ctx, userID)
	}
}

// announce publishes the profile as it is now. A failed read leaves the account
// unsent: the next update schedules another try, and a broken database must not
// turn into a stream of empty events.
func (s *Service) announce(ctx context.Context, userID uuid.UUID) {
	profile, err := s.Profile(ctx, userID)
	if err != nil {
		s.log.WarnContext(ctx, "read profile for user.updated", slog.Any("error", err))
		return
	}

	s.mu.Lock()
	s.sent[userID] = s.now()
	s.mu.Unlock()

	s.publish(ctx, userID, profile)
}

// publish tells everyone who already knows the account: its servers, the people
// in its conversations, its friends once they exist, and the account's own other
// connections. The payload is the same UserProfile the API returns, so a client
// replaces the author already in its feed.
func (s *Service) publish(ctx context.Context, userID uuid.UUID, profile Public) {
	if s.hub == nil {
		return
	}
	payload := APIProfile(profile)

	if s.servers != nil {
		servers, err := s.servers.Servers(ctx, userID)
		if err != nil {
			s.log.WarnContext(ctx, "list servers for user.updated", slog.Any("error", err))
		}
		for _, workspaceID := range servers {
			s.hub.Publish(events.Event{
				Kind:        events.KindUserUpdated,
				WorkspaceID: workspaceID,
				Payload:     payload,
			})
		}
	}

	// A set, so a friend who is also in a conversation gets one personal event.
	audience := map[uuid.UUID]bool{userID: true}
	if s.conversations != nil {
		peers, err := s.conversations.Peers(ctx, userID)
		if err != nil {
			s.log.WarnContext(ctx, "list conversation peers for user.updated", slog.Any("error", err))
		}
		for _, peerID := range peers {
			audience[peerID] = true
		}
	}
	if s.friends != nil {
		friends, err := s.friends.FriendsOf(ctx, userID)
		if err != nil {
			s.log.WarnContext(ctx, "list friends for user.updated", slog.Any("error", err))
		}
		for _, friendID := range friends {
			audience[friendID] = true
		}
	}
	for id := range audience {
		s.hub.PublishToUser(id, events.Event{
			Kind:    events.KindUserUpdated,
			Payload: payload,
		})
	}
}

// present turns a stored row into what the caller may see: an expired status is
// gone and the avatar key becomes a short-lived link.
func (s *Service) present(ctx context.Context, stored Stored) Public {
	profile := hideExpired(stored.Public, s.now())
	if profile.AvatarURL != "" || stored.AvatarKey == "" || s.avatars.Storage == nil {
		return profile
	}
	url, err := s.avatars.Storage.SignedInlineURL(ctx, avatarObjectKey(stored.AvatarKey, avatarLarge), "image/webp", s.avatars.LinkTTL)
	if err == nil {
		profile.AvatarURL = url
	}
	return profile
}
