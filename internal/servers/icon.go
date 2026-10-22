package servers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/perms"
)

var (
	// ErrIconTooLarge means the image is over this instance's limit.
	ErrIconTooLarge = errors.New("servers: the icon is too large")
	// ErrIconEmpty means there were no bytes to store.
	ErrIconEmpty = errors.New("servers: the icon is empty")
	// ErrIconType means the bytes are not a PNG, JPEG or WebP image.
	ErrIconType = errors.New("servers: the icon must be a PNG, JPEG or WebP image")
	// ErrIconsUnavailable means this instance has no object store for icons.
	ErrIconsUnavailable = errors.New("servers: icons are not configured")
)

// sniffBytes is what http.DetectContentType reads.
const sniffBytes = 512

// iconTypes are the images a server icon may be, by what the bytes say, with
// the extension the object key gets.
var iconTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/webp": ".webp",
}

// IconStorage is the object store behind the icons; objectstore.Client fits.
type IconStorage interface {
	Put(ctx context.Context, key, mimeType string, body io.Reader, size int64) error
	SignedURL(ctx context.Context, key, fileName string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

// Icons configures server icons. The zero value turns them off: no upload, and
// every server shows no icon.
type Icons struct {
	Storage  IconStorage
	MaxBytes int64
	// LinkTTL is how long an iconUrl stays valid.
	LinkTTL time.Duration
}

// MaxIconBytes is this instance's icon limit, so the API can say it plainly.
func (s *Service) MaxIconBytes() int64 { return s.icons.MaxBytes }

// SetIcon stores a new icon for the server and drops the old one. Admin or owner.
// The type is read from the bytes; the name the client gave is not asked.
func (s *Service) SetIcon(ctx context.Context, workspaceID, callerID uuid.UUID, body io.Reader) (Membership, error) {
	access, err := s.Require(ctx, workspaceID, callerID, perms.ManageServer)
	role := access.Role()
	if err != nil {
		return Membership{}, err
	}
	if s.icons.Storage == nil {
		return Membership{}, ErrIconsUnavailable
	}

	head := make([]byte, sniffBytes)
	read, err := io.ReadFull(body, head)
	switch {
	case errors.Is(err, io.EOF):
		return Membership{}, ErrIconEmpty
	case errors.Is(err, io.ErrUnexpectedEOF):
		head = head[:read]
	case err != nil:
		return Membership{}, fmt.Errorf("read icon: %w", err)
	}
	mimeType := http.DetectContentType(head)
	extension, ok := iconTypes[mimeType]
	if !ok {
		return Membership{}, ErrIconType
	}

	// One byte over the limit is enough to refuse: read no further.
	image, err := io.ReadAll(io.LimitReader(io.MultiReader(bytes.NewReader(head), body), s.icons.MaxBytes+1))
	if err != nil {
		return Membership{}, fmt.Errorf("read icon: %w", err)
	}
	if int64(len(image)) > s.icons.MaxBytes {
		return Membership{}, ErrIconTooLarge
	}

	// A fresh key per upload: a link to the old icon never shows the new one.
	key := fmt.Sprintf("workspaces/%s/icon/%s%s", workspaceID, uuid.NewString(), extension)
	if err := s.icons.Storage.Put(ctx, key, mimeType, bytes.NewReader(image), int64(len(image))); err != nil {
		return Membership{}, fmt.Errorf("store icon: %w", err)
	}
	previous, err := s.store.SetIcon(ctx, workspaceID, key)
	if err != nil {
		// Without the row nobody finds the object; take it back.
		s.dropIcon(ctx, key)
		return Membership{}, err
	}
	s.dropIcon(ctx, previous)
	return s.afterIconChange(ctx, workspaceID, role)
}

// RemoveIcon takes the icon away. Admin or owner; no icon is not an error.
func (s *Service) RemoveIcon(ctx context.Context, workspaceID, callerID uuid.UUID) (Membership, error) {
	access, err := s.Require(ctx, workspaceID, callerID, perms.ManageServer)
	role := access.Role()
	if err != nil {
		return Membership{}, err
	}
	previous, err := s.store.SetIcon(ctx, workspaceID, "")
	if err != nil {
		return Membership{}, err
	}
	s.dropIcon(ctx, previous)
	if previous == "" {
		// Nothing changed: nobody needs to hear about it.
		workspace, err := s.store.Workspace(ctx, workspaceID)
		if err != nil {
			return Membership{}, err
		}
		return Membership{Workspace: s.present(ctx, workspace), Role: role}, nil
	}
	return s.afterIconChange(ctx, workspaceID, role)
}

func (s *Service) afterIconChange(ctx context.Context, workspaceID uuid.UUID, role Role) (Membership, error) {
	workspace, err := s.store.Workspace(ctx, workspaceID)
	if err != nil {
		return Membership{}, err
	}
	s.changed(ctx, workspace)
	return Membership{Workspace: s.present(ctx, workspace), Role: role}, nil
}

// present fills in what only a member sees: the signed link to the icon. A link
// that cannot be signed leaves the icon out rather than failing the whole read.
func (s *Service) present(ctx context.Context, workspace Workspace) Workspace {
	workspace.IconURL = s.signIconURL(ctx, workspace.IconKey)
	return workspace
}

// signIconURL turns an icon key into a link the browser renders. The empty key
// and an instance without a store both mean no link; a signing failure is not a
// reason to fail a read that does not depend on the icon.
func (s *Service) signIconURL(ctx context.Context, key string) string {
	if key == "" || s.icons.Storage == nil {
		return ""
	}
	url, err := s.icons.Storage.SignedURL(ctx, key, "icon"+path.Ext(key), s.icons.LinkTTL)
	if err != nil {
		return ""
	}
	return url
}

// dropIcon deletes an icon object nobody points at any more. It is best effort:
// the row has already moved on, and a leftover object only costs space.
func (s *Service) dropIcon(ctx context.Context, key string) {
	if key == "" || s.icons.Storage == nil {
		return
	}
	_ = s.icons.Storage.Delete(ctx, key)
}
