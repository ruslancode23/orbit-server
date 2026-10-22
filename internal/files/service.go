// Package files owns attachments (FR-05): the upload goes through the server,
// which checks the size and reads the first bytes to see what the file really
// is, and downloads go straight from the object store by a signed link.
package files

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/servers"
)

var (
	// ErrNotFound covers a missing file and a channel the caller may not read.
	ErrNotFound = errors.New("files: not found")
	// ErrForbidden means the caller may read the channel but not write to it.
	ErrForbidden = errors.New("files: not allowed")
	// ErrAttachNotAllowed means the server does not let members attach files.
	ErrAttachNotAllowed = fmt.Errorf("%w: this server does not let members attach files", ErrForbidden)
	// ErrTooLarge means the file is over this instance's limit.
	ErrTooLarge = errors.New("files: the file is too large")
	// ErrEmpty means there were no bytes to store.
	ErrEmpty = errors.New("files: the file is empty")
	// ErrUnsupportedType means the content is not a kind we accept.
	ErrUnsupportedType = errors.New("files: this kind of file is not accepted")
)

const (
	// sniffBytes is what http.DetectContentType reads.
	sniffBytes = 512
	// maxFileNameLength keeps a pathological name out of the database.
	maxFileNameLength = 255
)

type Attachment struct {
	ID         uuid.UUID
	MessageID  *uuid.UUID
	UploaderID uuid.UUID
	// ChannelID is where the file was uploaded. A secret chat file has no
	// message: its recipients reach it through the channel.
	ChannelID  *uuid.UUID
	StorageKey string
	FileName   string
	MimeType   string
	SizeBytes  int64
	Width      *int
	Height     *int
	CreatedAt  time.Time
	// ExpiresAt is when the server forgets the file. Only secret chat files
	// have one.
	ExpiresAt *time.Time
}

// Link is a download: where to get the file and until when.
type Link struct {
	URL        string
	ExpiresAt  time.Time
	Attachment Attachment
}

type Store interface {
	Create(ctx context.Context, a Attachment) (Attachment, error)
	Attachment(ctx context.Context, id uuid.UUID) (Attachment, error)
	// Attach binds a file to a message; only its uploader may.
	Attach(ctx context.Context, id, messageID, uploaderID uuid.UUID) (Attachment, error)
	ByMessages(ctx context.Context, messageIDs []uuid.UUID) ([]Attachment, error)
	// Orphans are files whose message never arrived.
	Orphans(ctx context.Context, olderThan time.Time, limit int) ([]Attachment, error)
	Delete(ctx context.Context, id uuid.UUID) error
	// Expired are files whose deadline has passed, attached or not.
	Expired(ctx context.Context, limit int) ([]Attachment, error)
	// DeleteExpired removes a file only if its deadline has passed.
	DeleteExpired(ctx context.Context, id uuid.UUID) error
}

// Storage is the object store behind the files.
type Storage interface {
	Put(ctx context.Context, key, mimeType string, body io.Reader, size int64) error
	// SignedURL is a link that works for a short while and no longer.
	SignedURL(ctx context.Context, key, fileName string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

// ChannelAccess is what this package needs from internal/channels.
type ChannelAccess interface {
	MayWrite(ctx context.Context, channelID, callerID uuid.UUID) (channels.Channel, error)
	Get(ctx context.Context, channelID, callerID uuid.UUID) (channels.View, error)
}

// Permissions is the part of internal/servers that says whether a server lets
// this account attach files.
type Permissions interface {
	RequirePermission(ctx context.Context, workspaceID, userID uuid.UUID, permission servers.Permission) (servers.Role, error)
}

// Messages tells which channel a message belongs to, so a download link is
// issued only to someone who may read that channel.
type Messages interface {
	ChannelOf(ctx context.Context, messageID uuid.UUID) (uuid.UUID, error)
}

type Service struct {
	store       Store
	storage     Storage
	channels    ChannelAccess
	messages    Messages
	permissions Permissions
	maxBytes    int64
	linkTTL     time.Duration
}

func NewService(store Store, storage Storage, access ChannelAccess, msgs Messages, perms Permissions, maxBytes int64, linkTTL time.Duration) *Service {
	return &Service{
		store: store, storage: storage, channels: access, messages: msgs, permissions: perms,
		maxBytes: maxBytes, linkTTL: linkTTL,
	}
}

// MayAttach answers whether the account may attach files in this channel: in a
// server channel the server decides for members; in a conversation everyone in
// it may. The caller has already checked that the channel may be written in.
func (s *Service) MayAttach(ctx context.Context, channel channels.Channel, callerID uuid.UUID) error {
	if channel.WorkspaceID == uuid.Nil {
		return nil
	}
	if s.permissions == nil {
		// Unwired means unknown, and unknown is no.
		return ErrAttachNotAllowed
	}
	// The channel's own permissions: an override may allow or deny files here.
	view, err := s.channels.Get(ctx, channel.ID, callerID)
	switch {
	case errors.Is(err, channels.ErrNotFound), errors.Is(err, channels.ErrForbidden):
		return ErrNotFound
	case err != nil:
		return err
	}
	if !view.Permissions.Has(perms.AttachFiles) {
		return ErrAttachNotAllowed
	}
	return nil
}

// MaxBytes is this instance's upload limit, so the API can say it plainly.
func (s *Service) MaxBytes() int64 { return s.maxBytes }

// maxSecretFileTTL is the longest a secret chat file may outlive its upload:
// the same week a self-destruct timer can have.
const maxSecretFileTTL = 7 * 24 * time.Hour

// Upload stores one file for a channel the caller may write in. The file
// belongs to no message yet: a message names it when it is sent. A secret chat
// has no plaintext messages, so its files are bound to the channel and carry a
// deadline instead.
func (s *Service) Upload(ctx context.Context, channelID, uploaderID uuid.UUID, fileName string, body io.Reader, expiresAt *time.Time) (Attachment, error) {
	view, err := s.channels.Get(ctx, channelID, uploaderID)
	if err != nil {
		return Attachment{}, mapAccessError(err)
	}
	channel := view.Channel
	// A secret chat allows only ciphertext files; a message channel is written
	// the ordinary way.
	if channel.Kind != channels.KindSecret && !view.CanWrite {
		return Attachment{}, ErrForbidden
	}
	if err := s.MayAttach(ctx, channel, uploaderID); err != nil {
		return Attachment{}, err
	}

	// Read the head first: it decides the type, and the name has no say in it.
	head := make([]byte, sniffBytes)
	read, err := io.ReadFull(body, head)
	switch {
	case errors.Is(err, io.EOF):
		return Attachment{}, ErrEmpty
	case errors.Is(err, io.ErrUnexpectedEOF):
		head = head[:read]
	case err != nil:
		return Attachment{}, fmt.Errorf("read file: %w", err)
	}

	mimeType := http.DetectContentType(head)
	if !accepted(mimeType) {
		return Attachment{}, ErrUnsupportedType
	}

	// One byte over the limit is enough to refuse: read no further.
	counted := &countingReader{reader: io.MultiReader(bytes.NewReader(head), body), limit: s.maxBytes}
	buffered, err := io.ReadAll(counted)
	if errors.Is(err, errOverLimit) {
		return Attachment{}, ErrTooLarge
	}
	if err != nil {
		return Attachment{}, fmt.Errorf("read file: %w", err)
	}

	key := storageKey(channel.ID, fileName)
	if err := s.storage.Put(ctx, key, mimeType, bytes.NewReader(buffered), int64(len(buffered))); err != nil {
		return Attachment{}, err
	}

	attachment := Attachment{
		UploaderID: uploaderID,
		ChannelID:  &channel.ID,
		StorageKey: key,
		FileName:   cleanFileName(fileName),
		MimeType:   mimeType,
		SizeBytes:  int64(len(buffered)),
	}
	if channel.Kind == channels.KindSecret {
		attachment.ExpiresAt = secretFileExpiry(expiresAt)
	}

	stored, err := s.store.Create(ctx, attachment)
	if err != nil {
		// The row is what makes the file findable; without it, drop the object.
		_ = s.storage.Delete(ctx, key)
		return Attachment{}, err
	}
	return stored, nil
}

// Link issues a short-lived download for someone who may read the channel the
// file lives in. A file not yet attached is visible only to its uploader, and
// to the other participant of a secret chat, who has no message to point at.
func (s *Service) Link(ctx context.Context, attachmentID, callerID uuid.UUID) (Link, error) {
	attachment, err := s.store.Attachment(ctx, attachmentID)
	if err != nil {
		return Link{}, err
	}
	if attachment.ExpiresAt != nil && !time.Now().Before(*attachment.ExpiresAt) {
		// The server has not swept it yet, but it is already gone.
		return Link{}, ErrNotFound
	}

	if attachment.MessageID == nil {
		if attachment.UploaderID != callerID {
			if attachment.ChannelID == nil {
				return Link{}, ErrNotFound
			}
			view, err := s.channels.Get(ctx, *attachment.ChannelID, callerID)
			if err != nil {
				return Link{}, mapAccessError(err)
			}
			if view.Channel.Kind != channels.KindSecret {
				return Link{}, ErrNotFound
			}
		}
	} else {
		channelID, err := s.messages.ChannelOf(ctx, *attachment.MessageID)
		if err != nil {
			return Link{}, ErrNotFound
		}
		if _, err := s.channels.Get(ctx, channelID, callerID); err != nil {
			return Link{}, mapAccessError(err)
		}
	}

	url, err := s.storage.SignedURL(ctx, attachment.StorageKey, attachment.FileName, s.linkTTL)
	if err != nil {
		return Link{}, err
	}
	return Link{URL: url, ExpiresAt: time.Now().Add(s.linkTTL), Attachment: attachment}, nil
}

// AttachAll binds uploaded files to the message that names them. Only the
// uploader's own files, and only those still free, are taken.
func (s *Service) AttachAll(ctx context.Context, messageID, uploaderID uuid.UUID, ids []uuid.UUID) ([]Attachment, error) {
	out := make([]Attachment, 0, len(ids))
	for _, id := range ids {
		attachment, err := s.store.Attach(ctx, id, messageID, uploaderID)
		if errors.Is(err, ErrNotFound) {
			// Someone else's file, or one already used: leave it alone.
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, attachment)
	}
	return out, nil
}

// ByMessages loads the attachments of a page of messages in one query.
func (s *Service) ByMessages(ctx context.Context, messageIDs []uuid.UUID) ([]Attachment, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	return s.store.ByMessages(ctx, messageIDs)
}

// SweepExpired deletes secret chat files whose deadline has passed, object
// first, row second. A message timer deletes on the device; this is what makes
// the server forget too.
func (s *Service) SweepExpired(ctx context.Context, limit int) (int, error) {
	expired, err := s.store.Expired(ctx, limit)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, attachment := range expired {
		if err := s.storage.Delete(ctx, attachment.StorageKey); err != nil {
			return removed, err
		}
		if err := s.store.DeleteExpired(ctx, attachment.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// SweepOrphans deletes files whose message never arrived, object first, row
// second: a row without an object would point at nothing.
func (s *Service) SweepOrphans(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	orphans, err := s.store.Orphans(ctx, time.Now().Add(-olderThan), limit)
	if err != nil {
		return 0, err
	}

	removed := 0
	for _, attachment := range orphans {
		if err := s.storage.Delete(ctx, attachment.StorageKey); err != nil {
			return removed, err
		}
		if err := s.store.Delete(ctx, attachment.ID); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// accepted keeps out what a browser would run. Everything else is stored as a
// file people download, so the list is about safety, not about taste.
func accepted(mimeType string) bool {
	switch {
	case strings.HasPrefix(mimeType, "text/html"),
		strings.HasPrefix(mimeType, "text/xml"),
		strings.HasPrefix(mimeType, "image/svg"),
		strings.Contains(mimeType, "javascript"):
		return false
	}
	return true
}

// storageKey keeps the extension for the object store's own sake and makes the
// name unguessable: the link, not the path, is what grants access.
func storageKey(channelID uuid.UUID, fileName string) string {
	extension := strings.ToLower(filepath.Ext(fileName))
	if len(extension) > 10 {
		extension = ""
	}
	return fmt.Sprintf("channels/%s/%s%s", channelID, uuid.NewString(), extension)
}

func cleanFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == string(filepath.Separator) {
		name = "file"
	}
	if runes := []rune(name); len(runes) > maxFileNameLength {
		name = string(runes[:maxFileNameLength])
	}
	return name
}

func mapAccessError(err error) error {
	switch {
	case errors.Is(err, channels.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, channels.ErrForbidden):
		return ErrForbidden
	default:
		return err
	}
}

var errOverLimit = errors.New("files: over the limit")

// countingReader stops at one byte past the limit, so an oversized upload is
// refused without reading it all into memory.
type countingReader struct {
	reader io.Reader
	limit  int64
	read   int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		return n, errOverLimit
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("read file: %w", err)
	}
	return n, err //nolint:wrapcheck // io.EOF must reach the caller unwrapped
}

// secretFileExpiry is the file's deadline: what the client asked for, never
// longer than a week, and a week by default. A past date becomes a week: a
// client cannot make the server delete a file before it could be delivered.
func secretFileExpiry(requested *time.Time) *time.Time {
	deadline := time.Now().Add(maxSecretFileTTL)
	if requested != nil && requested.After(time.Now()) && requested.Before(deadline) {
		deadline = *requested
	}
	return &deadline
}
