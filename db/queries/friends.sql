-- Friendships (SCRUM-98). A pair has at most one row, stored with the smaller
-- account id first, so every lookup is one equality on the primary key. The
-- friends module owns this table; privacy asks it through a Go interface when a
-- block has to withdraw a request.

-- A request is sent by exact nickname, compared without case. The same rule as
-- registration's unique index, so "Anna" and "anna" name one account.
-- name: UserByNickname :one
SELECT id, nickname, is_guest FROM users WHERE lower(nickname) = lower(sqlc.arg(nickname)::text);

-- name: UserByID :one
SELECT id, nickname, is_guest FROM users WHERE id = $1;

-- The one row of a pair, with both nicknames: a hit is either a pending request
-- or a friendship, and the caller decides which way it may move.
-- name: FriendshipPair :one
SELECT f.user_low,
       f.user_high,
       f.status,
       f.requested_by,
       f.created_at,
       f.accepted_at,
       low.nickname  AS low_nickname,
       high.nickname AS high_nickname,
       low.is_guest  AS low_is_guest,
       high.is_guest AS high_is_guest
FROM friendships f
    JOIN users low ON low.id = f.user_low
    JOIN users high ON high.id = f.user_high
WHERE f.user_low = $1
  AND f.user_high = $2;

-- Sending twice is harmless: the pair already has its row and the request stays
-- as it was. The unique primary key is what decides a race, not a read first.
-- No row comes back when the insert lost that race.
-- name: CreateFriendshipRequest :one
INSERT INTO friendships (user_low, user_high, status, requested_by)
VALUES ($1, $2, 'pending', $3)
ON CONFLICT (user_low, user_high) DO NOTHING
RETURNING created_at;

-- A pending row becomes a friendship in place, so it keeps created_at and the
-- pair cannot end up with two rows. Only the pending row moves.
-- name: AcceptFriendship :one
UPDATE friendships
SET status      = 'accepted',
    accepted_at = now(),
    updated_at  = now()
WHERE user_low = $1
  AND user_high = $2
  AND status = 'pending'
RETURNING user_low, user_high, requested_by, created_at, accepted_at;

-- name: DeletePendingFriendship :execrows
DELETE FROM friendships
WHERE user_low = $1
  AND user_high = $2
  AND status = 'pending';

-- name: DeleteAcceptedFriendship :execrows
DELETE FROM friendships
WHERE user_low = $1
  AND user_high = $2
  AND status = 'accepted';

-- Blocking cancels whatever the pair had, pending or accepted. The caller reads
-- the row first so it can tell the two sides what disappeared and why.
-- name: DeleteFriendship :execrows
DELETE FROM friendships
WHERE user_low = $1
  AND user_high = $2;

-- Incoming requests: the other side sent it. The pair is normalized, so the
-- account may sit in either column; the CASE picks the other person.
-- name: ListIncomingFriendRequests :many
SELECT f.requested_by,
       f.created_at,
       u.id       AS user_id,
       u.nickname
FROM friendships f
    JOIN users u ON u.id = CASE WHEN f.user_low = $1 THEN f.user_high ELSE f.user_low END
WHERE f.status = 'pending'
  AND f.requested_by <> $1
  AND (f.user_low = $1 OR f.user_high = $1)
ORDER BY f.created_at DESC;

-- Outgoing requests: this account is the one that asked.
-- name: ListOutgoingFriendRequests :many
SELECT f.requested_by,
       f.created_at,
       u.id       AS user_id,
       u.nickname
FROM friendships f
    JOIN users u ON u.id = CASE WHEN f.user_low = $1 THEN f.user_high ELSE f.user_low END
WHERE f.status = 'pending'
  AND f.requested_by = $1
  AND (f.user_low = $1 OR f.user_high = $1)
ORDER BY f.created_at DESC;

-- name: ListFriends :many
SELECT u.id,
       u.nickname,
       f.accepted_at
FROM friendships f
    JOIN users u ON u.id = CASE WHEN f.user_low = $1 THEN f.user_high ELSE f.user_low END
WHERE f.status = 'accepted'
  AND (f.user_low = $1 OR f.user_high = $1)
ORDER BY lower(u.nickname);

-- The friend limit counts accepted rows only; a pending request does not take a
-- place in the list yet.
-- name: CountFriends :one
SELECT count(*)
FROM friendships
WHERE status = 'accepted'
  AND (user_low = $1 OR user_high = $1);

-- The pending limit counts only what this account sent; incoming requests are
-- the other side's limit to keep.
-- name: CountOutgoingPendingRequests :one
SELECT count(*)
FROM friendships
WHERE status = 'pending'
  AND requested_by = $1;

-- Presence asks who may see an account's status.
-- name: FriendsOf :many
SELECT (CASE WHEN user_low = $1 THEN user_high ELSE user_low END)::uuid AS friend_id
FROM friendships
WHERE status = 'accepted'
  AND (user_low = $1 OR user_high = $1);

-- privacy asks this before it lets a friendship open a dialogue.
-- name: AreFriends :one
SELECT EXISTS (
    SELECT 1
    FROM friendships
    WHERE status = 'accepted'
      AND user_low = LEAST(sqlc.arg(a)::uuid, sqlc.arg(b)::uuid)
      AND user_high = GREATEST(sqlc.arg(a)::uuid, sqlc.arg(b)::uuid)
);
