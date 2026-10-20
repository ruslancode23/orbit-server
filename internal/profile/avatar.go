package profile

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"

	// The standard decoders cover the two most common inputs; the WebP decoder
	// registers itself through the blank import below.
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"

	"github.com/HugoSmits86/nativewebp"
	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

var (
	// ErrAvatarEmpty means there were no bytes to store.
	ErrAvatarEmpty = errors.New("profile: the avatar is empty")
	// ErrAvatarTooLarge means the image is over this instance's byte limit.
	ErrAvatarTooLarge = errors.New("profile: the avatar is too large")
	// ErrAvatarType means the bytes are not a PNG, JPEG or WebP image.
	ErrAvatarType = errors.New("profile: the avatar must be a PNG, JPEG or WebP image")
	// ErrAvatarInvalid means the bytes look like an image but cannot be read.
	ErrAvatarInvalid = errors.New("profile: the image cannot be read")
	// ErrAvatarDimensions means the decoded image would be larger than this
	// server decodes: a small file can otherwise expand into a huge bitmap.
	ErrAvatarDimensions = errors.New("profile: the image is larger than 4096x4096 or 20 megapixels")
	// ErrAvatarsUnavailable means this instance has no object store for avatars.
	ErrAvatarsUnavailable = errors.New("profile: avatars are not configured")
)

const (
	// The two sizes every avatar is stored at. The larger is what UserProfile
	// links to; the smaller is for lists and message authors.
	avatarLarge = 256
	avatarSmall = 64
	// avatarSniffBytes is what http.DetectContentType reads.
	avatarSniffBytes = 512
	// maxAvatarDimension and maxAvatarPixels reject a decompression bomb from
	// its header, before the decoder allocates a bitmap from a tiny file.
	maxAvatarDimension = 4096
	maxAvatarPixels    = 20_000_000
)

// avatarContentTypes are the image types an avatar may be, by what the bytes
// say. GIF is absent on purpose: an animated avatar would need frame handling,
// and silently keeping the first frame loses what the person sent.
var avatarContentTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
}

var avatarDecodedFormats = map[string]bool{
	"png":  true,
	"jpeg": true,
	"webp": true,
}

// MaxAvatarBytes is this instance's avatar limit, so the API can say it plainly.
func (s *Service) MaxAvatarBytes() int64 { return s.avatars.MaxBytes }

// SetAvatar decodes an uploaded image, re-encodes it at both avatar sizes and
// points the account at the new key. The type comes from the bytes, never from
// the name the client gave. A fresh key per upload means a cached old link can
// never show the new picture; the old objects are deleted after the row moves.
func (s *Service) SetAvatar(ctx context.Context, userID uuid.UUID, body io.Reader) (Public, error) {
	if s.avatars.Storage == nil || s.avatars.MaxBytes <= 0 {
		return Public{}, ErrAvatarsUnavailable
	}

	// One byte over the limit is enough to refuse: read no further.
	data, err := io.ReadAll(io.LimitReader(body, s.avatars.MaxBytes+1))
	if err != nil {
		return Public{}, fmt.Errorf("read avatar: %w", err)
	}
	switch {
	case len(data) == 0:
		return Public{}, ErrAvatarEmpty
	case int64(len(data)) > s.avatars.MaxBytes:
		return Public{}, ErrAvatarTooLarge
	}

	encoded, err := encodeAvatar(data)
	if err != nil {
		return Public{}, err
	}

	key := fmt.Sprintf("avatars/%s/%s", userID, uuid.NewString())
	for _, size := range []int{avatarLarge, avatarSmall} {
		object := avatarObjectKey(key, size)
		if err := s.avatars.Storage.Put(ctx, object, "image/webp", bytes.NewReader(encoded[size]), int64(len(encoded[size]))); err != nil {
			s.dropAvatar(ctx, key)
			return Public{}, fmt.Errorf("store avatar: %w", err)
		}
	}

	stored, previous, err := s.store.SetAvatar(ctx, userID, key)
	if err != nil {
		// Without the row nobody finds the objects; take them back.
		s.dropAvatar(ctx, key)
		return Public{}, err
	}
	s.dropAvatar(ctx, previous)
	s.schedule(userID)
	return s.present(ctx, stored), nil
}

// ClearAvatar removes the avatar. Removing one that is not there is fine.
func (s *Service) ClearAvatar(ctx context.Context, userID uuid.UUID) (Public, error) {
	stored, previous, err := s.store.SetAvatar(ctx, userID, "")
	if err != nil {
		return Public{}, err
	}
	s.dropAvatar(ctx, previous)
	s.schedule(userID)
	return s.present(ctx, stored), nil
}

// ProfileChanged schedules a user.updated announcement without changing a
// profile field. internal/auth owns nickname and email and calls this after a
// successful change, so the same debounce and audience apply.
func (s *Service) ProfileChanged(_ context.Context, userID uuid.UUID) {
	s.schedule(userID)
}

// encodeAvatar validates the bytes and returns a WebP image for each stored
// size. Decoding and re-encoding also drops EXIF and every other metadata
// block, which is why the stored objects cannot leak a camera or a location.
func encodeAvatar(data []byte) (map[int][]byte, error) {
	head := data
	if len(head) > avatarSniffBytes {
		head = head[:avatarSniffBytes]
	}
	if !avatarContentTypes[http.DetectContentType(head)] {
		return nil, ErrAvatarType
	}

	// The header alone has the dimensions. Reading it before the full decode is
	// what stops a 50 000x50 000 PNG that weighs a few hundred bytes.
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || !avatarDecodedFormats[format] {
		return nil, ErrAvatarInvalid
	}
	if config.Width <= 0 || config.Height <= 0 ||
		config.Width > maxAvatarDimension || config.Height > maxAvatarDimension ||
		int64(config.Width)*int64(config.Height) > maxAvatarPixels {
		return nil, ErrAvatarDimensions
	}

	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrAvatarInvalid
	}

	encoded := make(map[int][]byte, 2)
	for _, size := range []int{avatarLarge, avatarSmall} {
		var buf bytes.Buffer
		if err := nativewebp.Encode(&buf, squareAvatar(source, size), nil); err != nil {
			return nil, fmt.Errorf("encode avatar: %w", err)
		}
		encoded[size] = buf.Bytes()
	}
	return encoded, nil
}

// squareAvatar centre-crops the image to a square and scales it to size. An
// avatar is drawn in a circle, so keeping the middle is what makes a portrait
// and a wide logo both look deliberate.
func squareAvatar(source image.Image, size int) image.Image {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	side := min(width, height)
	x0 := bounds.Min.X + (width-side)/2
	y0 := bounds.Min.Y + (height-side)/2

	destination := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.CatmullRom.Scale(destination, destination.Bounds(), source,
		image.Rect(x0, y0, x0+side, y0+side), draw.Src, nil)
	return destination
}

// avatarObjectKey is the key of one stored size. The base key in the row is
// shared by both objects, so a row move replaces both at once.
func avatarObjectKey(base string, size int) string {
	return fmt.Sprintf("%s-%d.webp", base, size)
}

// dropAvatar deletes both objects of an avatar nobody points at any more. It is
// best effort: the row has already moved on, and a leftover object only costs
// space.
func (s *Service) dropAvatar(ctx context.Context, key string) {
	if key == "" || s.avatars.Storage == nil {
		return
	}
	for _, size := range []int{avatarLarge, avatarSmall} {
		if err := s.avatars.Storage.Delete(ctx, avatarObjectKey(key, size)); err != nil {
			s.log.WarnContext(ctx, "delete avatar object", slog.String("key", avatarObjectKey(key, size)), slog.Any("error", err))
		}
	}
}
