package secrets

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

// Handler serves the generated device and secret chat operations; the rules
// live in Service.
type Handler struct {
	svc *Service
	log *slog.Logger
}

func NewHandler(svc *Service, log *slog.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

func caller(ctx context.Context) (uuid.UUID, bool) {
	session, ok := auth.SessionFrom(ctx)
	if !ok {
		return uuid.Nil, false
	}
	return session.User.ID, true
}

func (h *Handler) ListDevices(ctx context.Context, _ api.ListDevicesRequestObject) (api.ListDevicesResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListDevices401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	devices, err := h.svc.ListDevices(ctx, userID)
	if err != nil {
		h.log.ErrorContext(ctx, "list devices", slog.Any("error", err))
		return api.ListDevices500JSONResponse(internal("could not list the devices")), nil
	}
	return api.ListDevices200JSONResponse(toAPIDevices(devices)), nil
}

func (h *Handler) RegisterDevice(ctx context.Context, req api.RegisterDeviceRequestObject) (api.RegisterDeviceResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.RegisterDevice401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.RegisterDevice400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}

	name := ""
	if req.Body.Name != nil {
		name = *req.Body.Name
	}
	device, created, err := h.svc.RegisterDevice(ctx, userID, Registration{
		Name:        name,
		IdentityKey: req.Body.IdentityKey,
		SigningKey:  req.Body.SigningKey,
		Signature:   req.Body.Signature,
	})
	switch {
	case errors.Is(err, ErrInvalid):
		return api.RegisterDevice400JSONResponse{ErrorJSONResponse: badRequest("the keys or the signature do not check out")}, nil
	case errors.Is(err, ErrTooManyDevices):
		return api.RegisterDevice409JSONResponse(api.Error(conflict("this account already has as many devices as one instance allows"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "register device", slog.Any("error", err))
		return api.RegisterDevice500JSONResponse(internal("could not register the device")), nil
	}

	if !created {
		return api.RegisterDevice200JSONResponse(toAPIDevice(device)), nil
	}
	return api.RegisterDevice201JSONResponse(toAPIDevice(device)), nil
}

func (h *Handler) RevokeDevice(ctx context.Context, req api.RevokeDeviceRequestObject) (api.RevokeDeviceResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.RevokeDevice401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	err := h.svc.RevokeDevice(ctx, userID, req.DeviceId)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.RevokeDevice404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "revoke device", slog.Any("error", err))
		return api.RevokeDevice500JSONResponse(internal("could not revoke the device")), nil
	}
	return api.RevokeDevice204Response{}, nil
}

func (h *Handler) ListUserDevices(ctx context.Context, req api.ListUserDevicesRequestObject) (api.ListUserDevicesResponseObject, error) {
	if _, ok := caller(ctx); !ok {
		return api.ListUserDevices401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	devices, err := h.svc.UserDevices(ctx, req.UserId)
	switch {
	case errors.Is(err, ErrNotFound):
		return api.ListUserDevices404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list user devices", slog.Any("error", err))
		return api.ListUserDevices500JSONResponse(internal("could not list the devices")), nil
	}
	return api.ListUserDevices200JSONResponse(toAPIDevices(devices)), nil
}

func (h *Handler) UploadOneTimeKeys(ctx context.Context, req api.UploadOneTimeKeysRequestObject) (api.UploadOneTimeKeysResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.UploadOneTimeKeys401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.UploadOneTimeKeys400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}
	keys := make([]OneTimeKey, 0, len(req.Body.Keys))
	for _, key := range req.Body.Keys {
		keys = append(keys, OneTimeKey{KeyID: key.KeyId, Key: key.Key})
	}

	count, err := h.svc.UploadOneTimeKeys(ctx, userID, req.DeviceId, keys)
	switch {
	case errors.Is(err, ErrInvalid):
		return api.UploadOneTimeKeys400JSONResponse{ErrorJSONResponse: badRequest("the keys are malformed")}, nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.UploadOneTimeKeys404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrTooManyKeys):
		return api.UploadOneTimeKeys413JSONResponse(api.Error(tooMany("the device already holds as many one-time keys as one instance allows"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "upload one-time keys", slog.Any("error", err))
		return api.UploadOneTimeKeys500JSONResponse(internal("could not store the keys")), nil
	}
	return api.UploadOneTimeKeys200JSONResponse(api.OneTimeKeyCount{Count: count}), nil
}

func (h *Handler) CountOneTimeKeys(ctx context.Context, req api.CountOneTimeKeysRequestObject) (api.CountOneTimeKeysResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.CountOneTimeKeys401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	count, err := h.svc.CountOneTimeKeys(ctx, userID, req.DeviceId)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.CountOneTimeKeys404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "count one-time keys", slog.Any("error", err))
		return api.CountOneTimeKeys500JSONResponse(internal("could not count the keys")), nil
	}
	return api.CountOneTimeKeys200JSONResponse(api.OneTimeKeyCount{Count: count}), nil
}

func (h *Handler) GetKeyBundle(ctx context.Context, req api.GetKeyBundleRequestObject) (api.GetKeyBundleResponseObject, error) {
	if _, ok := caller(ctx); !ok {
		return api.GetKeyBundle401JSONResponse{ErrorJSONResponse: unauthorized()}, nil
	}
	device, key, err := h.svc.KeyBundle(ctx, req.DeviceId)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoKeys):
		return api.GetKeyBundle404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "get key bundle", slog.Any("error", err))
		return api.GetKeyBundle500JSONResponse(internal("could not read the keys")), nil
	}
	return api.GetKeyBundle200JSONResponse(api.KeyBundle{
		Device:     toAPIDevice(device),
		OneTimeKey: api.OneTimeKey{KeyId: key.KeyID, Key: key.Key},
	}), nil
}

func (h *Handler) ListSecretChats(ctx context.Context, req api.ListSecretChatsRequestObject) (api.ListSecretChatsResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListSecretChats401JSONResponse(api.Error(unauthorized())), nil
	}
	chats, err := h.svc.ListSecretChats(ctx, userID, req.Params.XOrbitDevice)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.ListSecretChats404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list secret chats", slog.Any("error", err))
		return api.ListSecretChats500JSONResponse(internal("could not list the secret chats")), nil
	}
	out := make(api.ListSecretChats200JSONResponse, 0, len(chats))
	for _, chat := range chats {
		out = append(out, toAPISecretChat(chat))
	}
	return out, nil
}

func (h *Handler) StartSecretChat(ctx context.Context, req api.StartSecretChatRequestObject) (api.StartSecretChatResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.StartSecretChat401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.StartSecretChat400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}
	session, _ := auth.SessionFrom(ctx)
	if session.User.IsGuest {
		return api.StartSecretChat403JSONResponse(api.Error(forbidden("a guest cannot start a secret chat"))), nil
	}

	chat, created, err := h.svc.StartSecretChat(ctx, userID, req.Params.XOrbitDevice, req.Body.UserId, req.Body.PeerDeviceId)
	switch {
	case errors.Is(err, ErrInvalid):
		return api.StartSecretChat400JSONResponse{ErrorJSONResponse: badRequest("name another person")}, nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNoDevices):
		return api.StartSecretChat404JSONResponse(api.Error(notFound())), nil
	case errors.Is(err, ErrForbidden):
		return api.StartSecretChat403JSONResponse(api.Error(forbidden("this device belongs to somebody else"))), nil
	case errors.Is(err, ErrManyDevices):
		return api.StartSecretChat409JSONResponse(api.Error(conflict("this person has several devices; name one"))), nil
	case err != nil:
		h.log.ErrorContext(ctx, "start secret chat", slog.Any("error", err))
		return api.StartSecretChat500JSONResponse(internal("could not start the secret chat")), nil
	}

	if !created {
		return api.StartSecretChat200JSONResponse(toAPISecretChat(chat)), nil
	}
	return api.StartSecretChat201JSONResponse(toAPISecretChat(chat)), nil
}

func (h *Handler) GetSecretChat(ctx context.Context, req api.GetSecretChatRequestObject) (api.GetSecretChatResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.GetSecretChat401JSONResponse(api.Error(unauthorized())), nil
	}
	chat, err := h.svc.Chat(ctx, userID, req.Params.XOrbitDevice, req.SecretChatId)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.GetSecretChat404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "get secret chat", slog.Any("error", err))
		return api.GetSecretChat500JSONResponse(internal("could not read the secret chat")), nil
	}
	return api.GetSecretChat200JSONResponse(toAPISecretChat(chat)), nil
}

func (h *Handler) LeaveSecretChat(ctx context.Context, req api.LeaveSecretChatRequestObject) (api.LeaveSecretChatResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.LeaveSecretChat401JSONResponse(api.Error(unauthorized())), nil
	}
	err := h.svc.LeaveChat(ctx, userID, req.Params.XOrbitDevice, req.SecretChatId)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.LeaveSecretChat404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "leave secret chat", slog.Any("error", err))
		return api.LeaveSecretChat500JSONResponse(internal("could not delete the secret chat")), nil
	}
	return api.LeaveSecretChat204Response{}, nil
}

func (h *Handler) ListSecretEnvelopes(ctx context.Context, req api.ListSecretEnvelopesRequestObject) (api.ListSecretEnvelopesResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.ListSecretEnvelopes401JSONResponse(api.Error(unauthorized())), nil
	}
	envelopes, err := h.svc.PendingEnvelopes(ctx, userID, req.Params.XOrbitDevice, req.SecretChatId)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.ListSecretEnvelopes404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "list envelopes", slog.Any("error", err))
		return api.ListSecretEnvelopes500JSONResponse(internal("could not read the envelopes")), nil
	}
	out := make(api.ListSecretEnvelopes200JSONResponse, 0, len(envelopes))
	for _, envelope := range envelopes {
		out = append(out, toAPIEnvelope(envelope))
	}
	return out, nil
}

func (h *Handler) SendSecretEnvelope(ctx context.Context, req api.SendSecretEnvelopeRequestObject) (api.SendSecretEnvelopeResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.SendSecretEnvelope401JSONResponse(api.Error(unauthorized())), nil
	}
	if req.Body == nil {
		return api.SendSecretEnvelope400JSONResponse{ErrorJSONResponse: badRequest("request body is required")}, nil
	}
	body, err := decodeBody(req.Body.Body)
	if err != nil {
		return api.SendSecretEnvelope400JSONResponse{ErrorJSONResponse: badRequest("the envelope must be base64")}, nil
	}

	envelope, err := h.svc.SendEnvelope(ctx, userID, req.Params.XOrbitDevice, req.SecretChatId, EnvelopeInput{
		RecipientDeviceID: req.Body.RecipientDeviceId,
		Type:              int(req.Body.Type),
		Body:              body,
		ExpiresAt:         req.Body.ExpiresAt,
	})
	switch {
	case errors.Is(err, ErrInvalid):
		return api.SendSecretEnvelope400JSONResponse{ErrorJSONResponse: badRequest("the envelope is malformed or its deadline is out of range")}, nil
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.SendSecretEnvelope404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "send envelope", slog.Any("error", err))
		return api.SendSecretEnvelope500JSONResponse(internal("could not store the envelope")), nil
	}
	return api.SendSecretEnvelope201JSONResponse(toAPIEnvelope(envelope)), nil
}

func (h *Handler) AcknowledgeSecretEnvelope(ctx context.Context, req api.AcknowledgeSecretEnvelopeRequestObject) (api.AcknowledgeSecretEnvelopeResponseObject, error) {
	userID, ok := caller(ctx)
	if !ok {
		return api.AcknowledgeSecretEnvelope401JSONResponse(api.Error(unauthorized())), nil
	}
	err := h.svc.AcknowledgeEnvelope(ctx, userID, req.Params.XOrbitDevice, req.EnvelopeId)
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrForbidden):
		return api.AcknowledgeSecretEnvelope404JSONResponse(api.Error(notFound())), nil
	case err != nil:
		h.log.ErrorContext(ctx, "acknowledge envelope", slog.Any("error", err))
		return api.AcknowledgeSecretEnvelope500JSONResponse(internal("could not delete the envelope")), nil
	}
	return api.AcknowledgeSecretEnvelope204Response{}, nil
}

func toAPIDevice(device Device) api.Device {
	return api.Device{
		Id:          device.ID,
		UserId:      device.UserID,
		Name:        device.Name,
		IdentityKey: device.IdentityKey,
		SigningKey:  device.SigningKey,
		CreatedAt:   device.CreatedAt,
	}
}

func toAPIDevices(devices []Device) []api.Device {
	out := make([]api.Device, 0, len(devices))
	for _, device := range devices {
		out = append(out, toAPIDevice(device))
	}
	return out
}

func toAPISecretChat(chat SecretChat) api.SecretChat {
	return api.SecretChat{
		Id:        chat.ID,
		CreatedAt: chat.CreatedAt,
		Peer: api.SecretChatPeer{
			UserId:      chat.Peer.UserID,
			Nickname:    chat.Peer.Nickname,
			IsGuest:     chat.Peer.IsGuest,
			DeviceId:    chat.Peer.DeviceID,
			IdentityKey: chat.Peer.IdentityKey,
			SigningKey:  chat.Peer.SigningKey,
		},
	}
}

func toAPIEnvelope(envelope Envelope) api.SecretEnvelope {
	return api.SecretEnvelope{
		Id:                envelope.ID,
		SecretChatId:      envelope.ChatID,
		SenderDeviceId:    envelope.SenderDeviceID,
		RecipientDeviceId: envelope.RecipientDeviceID,
		Type:              api.SecretEnvelopeType(envelope.Type),
		Body:              base64.StdEncoding.EncodeToString(envelope.Body),
		CreatedAt:         envelope.CreatedAt,
		ExpiresAt:         envelope.ExpiresAt,
	}
}

// decodeBody accepts standard and unpadded base64: clients pad differently.
func decodeBody(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return raw, nil
}

func unauthorized() api.ErrorJSONResponse {
	return errorBody(httpx.CodeUnauthorized, "sign in first")
}

func notFound() api.ErrorJSONResponse {
	return errorBody(httpx.CodeNotFound, "not found")
}

func forbidden(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeForbidden, message)
}

func badRequest(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeInvalidRequest, message)
}

func conflict(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeConflict, message)
}

func tooMany(message string) api.ErrorJSONResponse {
	return errorBody(httpx.CodeBodyTooLarge, message)
}

func internal(message string) api.Error {
	return api.Error(errorBody(httpx.CodeInternal, message))
}

func errorBody(code, message string) api.ErrorJSONResponse {
	return api.ErrorJSONResponse{Error: api.ErrorDetail{Code: code, Message: message}}
}
