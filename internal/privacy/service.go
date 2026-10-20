// Package privacy owns who may start a direct conversation with an account and
// whom people block (SCRUM-85). It owns the users.dm_policy column and the
// user_blocks table; conversations and messages ask it through the narrow
// interfaces they declare, so a rule is written once and checked everywhere.
package privacy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrNotFound covers an unknown account and one the caller may not talk to:
	// the same answer on purpose, or the API tells strangers which accounts exist.
	ErrNotFound = errors.New("privacy: not found")
	// ErrCannotBlockSelf means the caller named their own account.
	ErrCannotBlockSelf = errors.New("privacy: you cannot block yourself")
	// ErrInvalidPolicy means the value is not one of shared, friends, nobody.
	ErrInvalidPolicy = errors.New("privacy: unknown direct message policy")
)

// Policy is who may start a direct conversation with an account.
type Policy string

const (
	// PolicyShared lets people on a common server start a dialogue: the default.
	PolicyShared Policy = "shared"
	// PolicyFriends is reserved for the friendships of SCRUM-98. Nobody can be a
	// friend yet, so today it behaves exactly as PolicyNobody; keeping it in the
	// type and the database means the setting can ship before the feature.
	PolicyFriends Policy = "friends"
	// PolicyNobody lets nobody start a dialogue.
	PolicyNobody Policy = "nobody"
)

// Valid reports whether the policy is one this server knows.
func (p Policy) Valid() bool {
	return p == PolicyShared || p == PolicyFriends || p == PolicyNobody
}

// ParsePolicy turns a wire value into a policy, rejecting anything unknown.
func ParsePolicy(value string) (Policy, error) {
	policy := Policy(value)
	if !policy.Valid() {
		return "", ErrInvalidPolicy
	}
	return policy, nil
}

// BlockedUser is one account the caller blocked, as the settings screen shows it.
type BlockedUser struct {
	ID        uuid.UUID
	Nickname  string
	BlockedAt time.Time
}

// Account is what the rules need to know about one person.
type Account struct {
	Policy  Policy
	IsGuest bool
}

// Store is the persistence this package needs.
type Store interface {
	// UserPrivacy returns the policy and the guest flag of these accounts; an
	// unknown id is simply absent.
	UserPrivacy(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Account, error)
	// HasSharedWorkspace reports whether the two accounts are on one server.
	HasSharedWorkspace(ctx context.Context, a, b uuid.UUID) (bool, error)
	// BlockedEitherWay reports a block in either direction.
	BlockedEitherWay(ctx context.Context, a, b uuid.UUID) (bool, error)
	// BlockedIDs is whom this account blocked.
	BlockedIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)
	ListBlocks(ctx context.Context, userID uuid.UUID) ([]BlockedUser, error)
	Block(ctx context.Context, blockerID, blockedID uuid.UUID) error
	Unblock(ctx context.Context, blockerID, blockedID uuid.UUID) error
	UserExists(ctx context.Context, id uuid.UUID) (bool, error)
	Policy(ctx context.Context, userID uuid.UUID) (Policy, error)
	SetPolicy(ctx context.Context, userID uuid.UUID, policy Policy) error
}

// Service holds the rules. It knows nothing about HTTP.
type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

// CanStartDirect reports whether fromID may open a dialogue with toID. It is
// false when either account is missing or does not exist, the sender is a
// guest, a block exists in either direction, or the recipient's policy does not
// allow it. A guest may still answer in a dialogue that already exists: this is
// about starting one.
func (s *Service) CanStartDirect(ctx context.Context, fromID, toID uuid.UUID) (bool, error) {
	if fromID == toID || fromID == uuid.Nil || toID == uuid.Nil {
		return false, nil
	}

	accounts, err := s.store.UserPrivacy(ctx, []uuid.UUID{fromID, toID})
	if err != nil {
		return false, fmt.Errorf("read user privacy: %w", err)
	}
	from, ok := accounts[fromID]
	if !ok {
		return false, nil
	}
	to, ok := accounts[toID]
	if !ok {
		return false, nil
	}

	// A block outranks the policy and works both ways: neither side starts.
	blocked, err := s.BlockedEitherWay(ctx, fromID, toID)
	if err != nil {
		return false, err
	}
	if blocked {
		return false, nil
	}
	// A guest has no standing to open a dialogue with anyone.
	if from.IsGuest {
		return false, nil
	}

	switch to.Policy {
	case PolicyShared:
		shared, err := s.store.HasSharedWorkspace(ctx, fromID, toID)
		if err != nil {
			return false, fmt.Errorf("check shared workspace: %w", err)
		}
		return shared, nil
	case PolicyFriends, PolicyNobody:
		// Friends arrive with SCRUM-98; until then nobody is one.
		return false, nil
	default:
		// A value the database should not hold still must not open the door.
		return false, nil
	}
}

// CanStartGroup reports whether the caller may open a group with these people.
// A guest cannot start a group either, and nobody may add an account that is
// blocked either way. The recipient's dm_policy is about direct dialogues, so
// it does not decide about a group.
func (s *Service) CanStartGroup(ctx context.Context, callerID uuid.UUID, memberIDs []uuid.UUID) (bool, error) {
	if callerID == uuid.Nil || len(memberIDs) == 0 {
		return false, nil
	}

	ids := make([]uuid.UUID, 0, len(memberIDs)+1)
	ids = append(ids, callerID)
	ids = append(ids, memberIDs...)
	accounts, err := s.store.UserPrivacy(ctx, ids)
	if err != nil {
		return false, fmt.Errorf("read user privacy: %w", err)
	}
	caller, ok := accounts[callerID]
	if !ok || caller.IsGuest {
		return false, nil
	}

	for _, id := range memberIDs {
		if id == uuid.Nil {
			return false, nil
		}
		if _, ok := accounts[id]; !ok {
			return false, nil
		}
		blocked, err := s.BlockedEitherWay(ctx, callerID, id)
		if err != nil {
			return false, err
		}
		if blocked {
			return false, nil
		}
	}
	return true, nil
}

// BlockedEitherWay reports a block between the two accounts, in either
// direction. Both sides of an existing dialogue stop writing then.
func (s *Service) BlockedEitherWay(ctx context.Context, a, b uuid.UUID) (bool, error) {
	if a == b || a == uuid.Nil || b == uuid.Nil {
		return false, nil
	}
	blocked, err := s.store.BlockedEitherWay(ctx, a, b)
	if err != nil {
		return false, fmt.Errorf("check block: %w", err)
	}
	return blocked, nil
}

// BlockedIDs is whom this account blocked, as a set: history and live group
// events mark the authors in it.
func (s *Service) BlockedIDs(ctx context.Context, userID uuid.UUID) (map[uuid.UUID]bool, error) {
	ids, err := s.store.BlockedIDs(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list blocked ids: %w", err)
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// ListBlocks is the caller's own block list, most recent first. There is no
// way to read somebody else's: that would reveal whom they avoid.
func (s *Service) ListBlocks(ctx context.Context, userID uuid.UUID) ([]BlockedUser, error) {
	blocked, err := s.store.ListBlocks(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list blocks: %w", err)
	}
	return blocked, nil
}

// Block stops the dialogue in both directions, starting included. Blocking
// twice is fine, so a retry after a lost response is harmless.
func (s *Service) Block(ctx context.Context, blockerID, blockedID uuid.UUID) error {
	if blockerID == blockedID {
		return ErrCannotBlockSelf
	}
	if blockerID == uuid.Nil || blockedID == uuid.Nil {
		return ErrNotFound
	}
	exists, err := s.store.UserExists(ctx, blockedID)
	if err != nil {
		return fmt.Errorf("check blocked account: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	if err := s.store.Block(ctx, blockerID, blockedID); err != nil {
		return fmt.Errorf("block account: %w", err)
	}
	return nil
}

// Unblock lifts the block. Unblocking an account that is not blocked is fine:
// the client retries without having to know the current state.
func (s *Service) Unblock(ctx context.Context, blockerID, blockedID uuid.UUID) error {
	if err := s.store.Unblock(ctx, blockerID, blockedID); err != nil {
		return fmt.Errorf("unblock account: %w", err)
	}
	return nil
}

// Policy is the account's current setting.
func (s *Service) Policy(ctx context.Context, userID uuid.UUID) (Policy, error) {
	policy, err := s.store.Policy(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("read dm policy: %w", err)
	}
	return policy, nil
}

// SetPolicy changes who may start a direct conversation with the account.
func (s *Service) SetPolicy(ctx context.Context, userID uuid.UUID, policy Policy) error {
	if !policy.Valid() {
		return ErrInvalidPolicy
	}
	if err := s.store.SetPolicy(ctx, userID, policy); err != nil {
		return fmt.Errorf("set dm policy: %w", err)
	}
	return nil
}
