// Package httpserver wires HTTP routing and shared middleware.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/channels"
	"github.com/ruslancode23/orbit-server/internal/conversations"
	"github.com/ruslancode23/orbit-server/internal/files"
	"github.com/ruslancode23/orbit-server/internal/friends"
	"github.com/ruslancode23/orbit-server/internal/health"
	"github.com/ruslancode23/orbit-server/internal/invites"
	"github.com/ruslancode23/orbit-server/internal/messages"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
	"github.com/ruslancode23/orbit-server/internal/privacy"
	"github.com/ruslancode23/orbit-server/internal/profile"
	"github.com/ruslancode23/orbit-server/internal/reads"
	"github.com/ruslancode23/orbit-server/internal/secrets"
	"github.com/ruslancode23/orbit-server/internal/servers"
	"github.com/ruslancode23/orbit-server/internal/voice"
)

// maxBodyBytes caps every JSON request body. The largest is a message of 4000
// characters, up to four bytes each; uploads have their own limits.
const maxBodyBytes = 16 << 10

// envelopeBodyBytes caps a ciphertext envelope: 64 KiB of bytes, base64-encoded,
// plus the JSON around it.
const envelopeBodyBytes = 128 << 10

// multipartSlack is room for the multipart headers around an icon, so an image
// of exactly the limit is refused by the icon rule, with its own message, and
// not by the body cap.
const multipartSlack = 4 << 10

// wsPath is the live stream. It is not in api/openapi.yaml: the spec describes
// HTTP requests and responses, and a WebSocket is neither.
const wsPath = "/api/v1/ws"

// voiceWebhookPath is where LiveKit posts room events. Its body is signed, not
// session-authenticated, so it sits outside the OpenAPI routes.
const voiceWebhookPath = "/api/v1/livekit/webhook"

type Deps struct {
	Logger      *slog.Logger
	CORSOrigins []string
	Health      *health.Handler
	// Auth serves accounts and sessions; its middleware resolves the session cookie.
	Auth     *auth.Handler
	Sessions auth.Resolver
	// Workspaces serves servers, members and roles.
	Workspaces *servers.Handler
	// Channels serves the text channels of a server.
	Channels *channels.Handler
	// Messages serves what people write in those channels.
	Messages *messages.Handler
	// Invites serves invite links and the guest entrance.
	Invites *invites.Handler
	// Reads serves read cursors and unread counts.
	Reads *reads.Handler
	// Files serves attachments.
	Files *files.Handler
	// Conversations serves direct messages and group chats.
	Conversations *conversations.Handler
	// Secrets serves devices, their public keys and ciphertext envelopes.
	Secrets *secrets.Handler
	// Privacy serves block lists, the direct message policy and the friendship
	// request policy.
	Privacy *privacy.Handler
	// Profiles serves display names, bios, accent colours and custom statuses.
	Profiles *profile.Handler
	// Friends serves friendship requests and the friend list.
	Friends *friends.Handler
	// MaxUploadBytes is how large an attachment may be; the body limit for the
	// upload route is set from it.
	MaxUploadBytes int64
	// MaxIconBytes is how large a server icon may be, the same way.
	MaxIconBytes int64
	// MaxAvatarBytes is how large an avatar may be, the same way.
	MaxAvatarBytes int64
	// Gateway serves the WebSocket stream; it is outside the OpenAPI contract,
	// so it is mounted beside the generated routes, not through them.
	Gateway http.Handler
	// Voice issues LiveKit tokens for voice channels; its grants follow the caller's role.
	Voice *voice.Handler
	// VoiceWebhook receives LiveKit room events; nil disables presence updates.
	VoiceWebhook http.Handler
}

// NewRouter serves the API described by api/openapi.yaml. Every request is
// validated against the spec before it reaches a handler.
func NewRouter(d Deps) (http.Handler, error) {
	// A nil auth dependency would silently turn every session check into a panic
	// (or worse, into open access): refuse to build such a router.
	if d.Auth == nil || d.Sessions == nil || d.Workspaces == nil || d.Channels == nil || d.Messages == nil || d.Invites == nil || d.Reads == nil || d.Files == nil || d.Conversations == nil || d.Secrets == nil || d.Privacy == nil || d.Profiles == nil || d.Friends == nil || d.Voice == nil {
		return nil, errors.New("httpserver: every module handler is required: Auth, Sessions, Workspaces, Channels, Messages, Invites, Reads")
	}

	spec, err := api.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI spec: %w", err)
	}
	spec.Servers = nil // validate paths only, whatever host the server runs on

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(d.Logger))
	r.Use(middleware.Recoverer)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   d.CORSOrigins,
		AllowedMethods:   []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Orbit-Device"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	// Before validation: a handler reads the session from the context, and the
	// WebSocket upgrade will need it too.
	r.Use(auth.Middleware(d.Sessions, d.Logger))
	// Anything outside both groups answers in the API's error format, not chi's
	// bare 404: a client parses one shape of error whatever it asked for.
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		httpx.WriteError(w, http.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed, "method not allowed")
	})

	// The WebSocket route shares the middleware above but takes neither the body
	// limit nor the spec validator: an upgrade is not a request the spec describes.
	// It is registered first so chi matches this exact path before the API routes.
	if d.Gateway != nil {
		r.Group(func(ws chi.Router) {
			ws.Use(auth.RequireSession)
			ws.Handle(wsPath, d.Gateway)
		})
	}
	if d.VoiceWebhook != nil {
		r.Post(voiceWebhookPath, d.VoiceWebhook.ServeHTTP)
	}

	strict := api.NewStrictHandlerWithOptions(&server{health: d.Health, auth: d.Auth, workspaces: d.Workspaces, channels: d.Channels, messages: d.Messages, invites: d.Invites, reads: d.Reads, files: d.Files, conversations: d.Conversations, secrets: d.Secrets, privacy: d.Privacy, profiles: d.Profiles, friends: d.Friends, voice: d.Voice}, nil, api.StrictHTTPServerOptions{
		RequestErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
		},
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			d.Logger.ErrorContext(r.Context(), "handler failed", slog.Any("error", err))
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		},
	})
	// Everything the spec describes goes through validation, with the body limit.
	r.Group(func(apiRoutes chi.Router) {
		// JSON bodies stay tiny; only an upload may be large, and never larger
		// than this instance allows.
		apiRoutes.Use(limitBody(maxBodyBytes, d.MaxUploadBytes, d.MaxIconBytes, d.MaxAvatarBytes))
		apiRoutes.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
			DoNotValidateServers: true,
			ErrorHandlerWithOpts: validationError,
			Options: openapi3filter.Options{
				// Keep messages short: no schema dump in responses.
				SchemaValidationOptions: []openapi3.SchemaValidationOption{openapi3.DisableSchemaErrorDetails()},
			},
		}))
		api.HandlerWithOptions(strict, api.ChiServerOptions{
			BaseRouter: apiRoutes,
			ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
				httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())
			},
		})
	})
	return r, nil
}

// server assembles the module handlers into the generated interface.
type server struct {
	health        *health.Handler
	auth          *auth.Handler
	workspaces    *servers.Handler
	channels      *channels.Handler
	messages      *messages.Handler
	invites       *invites.Handler
	reads         *reads.Handler
	files         *files.Handler
	conversations *conversations.Handler
	secrets       *secrets.Handler
	privacy       *privacy.Handler
	profiles      *profile.Handler
	friends       *friends.Handler
	voice         *voice.Handler
}

var _ api.StrictServerInterface = (*server)(nil)

func (s *server) GetLiveness(ctx context.Context, req api.GetLivenessRequestObject) (api.GetLivenessResponseObject, error) {
	return s.health.GetLiveness(ctx, req)
}

func (s *server) GetReadiness(ctx context.Context, req api.GetReadinessRequestObject) (api.GetReadinessResponseObject, error) {
	return s.health.GetReadiness(ctx, req)
}

func (s *server) CreateChannelVoiceToken(ctx context.Context, req api.CreateChannelVoiceTokenRequestObject) (api.CreateChannelVoiceTokenResponseObject, error) {
	return s.voice.CreateChannelVoiceToken(ctx, req)
}

func (s *server) Register(ctx context.Context, req api.RegisterRequestObject) (api.RegisterResponseObject, error) {
	return s.auth.Register(ctx, req)
}

func (s *server) Login(ctx context.Context, req api.LoginRequestObject) (api.LoginResponseObject, error) {
	return s.auth.Login(ctx, req)
}

func (s *server) Logout(ctx context.Context, req api.LogoutRequestObject) (api.LogoutResponseObject, error) {
	return s.auth.Logout(ctx, req)
}

func (s *server) GetCurrentUser(ctx context.Context, req api.GetCurrentUserRequestObject) (api.GetCurrentUserResponseObject, error) {
	return s.auth.GetCurrentUser(ctx, req)
}

func (s *server) ChangePassword(ctx context.Context, req api.ChangePasswordRequestObject) (api.ChangePasswordResponseObject, error) {
	return s.auth.ChangePassword(ctx, req)
}

func (s *server) ListWorkspaces(ctx context.Context, req api.ListWorkspacesRequestObject) (api.ListWorkspacesResponseObject, error) {
	return s.workspaces.ListWorkspaces(ctx, req)
}

func (s *server) CreateWorkspace(ctx context.Context, req api.CreateWorkspaceRequestObject) (api.CreateWorkspaceResponseObject, error) {
	return s.workspaces.CreateWorkspace(ctx, req)
}

func (s *server) GetWorkspace(ctx context.Context, req api.GetWorkspaceRequestObject) (api.GetWorkspaceResponseObject, error) {
	return s.workspaces.GetWorkspace(ctx, req)
}

func (s *server) UpdateWorkspace(ctx context.Context, req api.UpdateWorkspaceRequestObject) (api.UpdateWorkspaceResponseObject, error) {
	return s.workspaces.UpdateWorkspace(ctx, req)
}

func (s *server) UploadWorkspaceIcon(ctx context.Context, req api.UploadWorkspaceIconRequestObject) (api.UploadWorkspaceIconResponseObject, error) {
	return s.workspaces.UploadWorkspaceIcon(ctx, req)
}

func (s *server) DeleteWorkspaceIcon(ctx context.Context, req api.DeleteWorkspaceIconRequestObject) (api.DeleteWorkspaceIconResponseObject, error) {
	return s.workspaces.DeleteWorkspaceIcon(ctx, req)
}

func (s *server) DeleteWorkspace(ctx context.Context, req api.DeleteWorkspaceRequestObject) (api.DeleteWorkspaceResponseObject, error) {
	return s.workspaces.DeleteWorkspace(ctx, req)
}

func (s *server) ListWorkspaceMembers(ctx context.Context, req api.ListWorkspaceMembersRequestObject) (api.ListWorkspaceMembersResponseObject, error) {
	return s.workspaces.ListWorkspaceMembers(ctx, req)
}

func (s *server) SetWorkspaceMemberRole(ctx context.Context, req api.SetWorkspaceMemberRoleRequestObject) (api.SetWorkspaceMemberRoleResponseObject, error) {
	return s.workspaces.SetWorkspaceMemberRole(ctx, req)
}

func (s *server) RemoveWorkspaceMember(ctx context.Context, req api.RemoveWorkspaceMemberRequestObject) (api.RemoveWorkspaceMemberResponseObject, error) {
	return s.workspaces.RemoveWorkspaceMember(ctx, req)
}

func (s *server) BanWorkspaceMember(ctx context.Context, req api.BanWorkspaceMemberRequestObject) (api.BanWorkspaceMemberResponseObject, error) {
	return s.workspaces.BanWorkspaceMember(ctx, req)
}

func (s *server) UnbanWorkspaceMember(ctx context.Context, req api.UnbanWorkspaceMemberRequestObject) (api.UnbanWorkspaceMemberResponseObject, error) {
	return s.workspaces.UnbanWorkspaceMember(ctx, req)
}

func (s *server) ListWorkspaceBans(ctx context.Context, req api.ListWorkspaceBansRequestObject) (api.ListWorkspaceBansResponseObject, error) {
	return s.workspaces.ListWorkspaceBans(ctx, req)
}

func (s *server) ListChannels(ctx context.Context, req api.ListChannelsRequestObject) (api.ListChannelsResponseObject, error) {
	return s.channels.ListChannels(ctx, req)
}

func (s *server) CreateChannel(ctx context.Context, req api.CreateChannelRequestObject) (api.CreateChannelResponseObject, error) {
	return s.channels.CreateChannel(ctx, req)
}

func (s *server) GetChannel(ctx context.Context, req api.GetChannelRequestObject) (api.GetChannelResponseObject, error) {
	return s.channels.GetChannel(ctx, req)
}

func (s *server) UpdateChannel(ctx context.Context, req api.UpdateChannelRequestObject) (api.UpdateChannelResponseObject, error) {
	return s.channels.UpdateChannel(ctx, req)
}

func (s *server) DeleteChannel(ctx context.Context, req api.DeleteChannelRequestObject) (api.DeleteChannelResponseObject, error) {
	return s.channels.DeleteChannel(ctx, req)
}

func (s *server) ReorderChannels(ctx context.Context, req api.ReorderChannelsRequestObject) (api.ReorderChannelsResponseObject, error) {
	return s.channels.ReorderChannels(ctx, req)
}

func (s *server) ListMessages(ctx context.Context, req api.ListMessagesRequestObject) (api.ListMessagesResponseObject, error) {
	return s.messages.ListMessages(ctx, req)
}

func (s *server) SendMessage(ctx context.Context, req api.SendMessageRequestObject) (api.SendMessageResponseObject, error) {
	return s.messages.SendMessage(ctx, req)
}

func (s *server) EditMessage(ctx context.Context, req api.EditMessageRequestObject) (api.EditMessageResponseObject, error) {
	return s.messages.EditMessage(ctx, req)
}

func (s *server) DeleteMessage(ctx context.Context, req api.DeleteMessageRequestObject) (api.DeleteMessageResponseObject, error) {
	return s.messages.DeleteMessage(ctx, req)
}

func (s *server) AddReaction(ctx context.Context, req api.AddReactionRequestObject) (api.AddReactionResponseObject, error) {
	return s.messages.AddReaction(ctx, req)
}

func (s *server) RemoveReaction(ctx context.Context, req api.RemoveReactionRequestObject) (api.RemoveReactionResponseObject, error) {
	return s.messages.RemoveReaction(ctx, req)
}

func (s *server) CreateInvite(ctx context.Context, req api.CreateInviteRequestObject) (api.CreateInviteResponseObject, error) {
	return s.invites.CreateInvite(ctx, req)
}

func (s *server) ListInvites(ctx context.Context, req api.ListInvitesRequestObject) (api.ListInvitesResponseObject, error) {
	return s.invites.ListInvites(ctx, req)
}

func (s *server) RevokeInvite(ctx context.Context, req api.RevokeInviteRequestObject) (api.RevokeInviteResponseObject, error) {
	return s.invites.RevokeInvite(ctx, req)
}

func (s *server) PreviewInvite(ctx context.Context, req api.PreviewInviteRequestObject) (api.PreviewInviteResponseObject, error) {
	return s.invites.PreviewInvite(ctx, req)
}

func (s *server) AcceptInvite(ctx context.Context, req api.AcceptInviteRequestObject) (api.AcceptInviteResponseObject, error) {
	return s.invites.AcceptInvite(ctx, req)
}

func (s *server) JoinAsGuest(ctx context.Context, req api.JoinAsGuestRequestObject) (api.JoinAsGuestResponseObject, error) {
	return s.invites.JoinAsGuest(ctx, req)
}

func (s *server) ClaimGuestAccount(ctx context.Context, req api.ClaimGuestAccountRequestObject) (api.ClaimGuestAccountResponseObject, error) {
	return s.auth.ClaimGuestAccount(ctx, req)
}

func (s *server) ListUnread(ctx context.Context, req api.ListUnreadRequestObject) (api.ListUnreadResponseObject, error) {
	return s.reads.ListUnread(ctx, req)
}

func (s *server) MarkChannelRead(ctx context.Context, req api.MarkChannelReadRequestObject) (api.MarkChannelReadResponseObject, error) {
	return s.reads.MarkChannelRead(ctx, req)
}

func (s *server) UploadAttachment(ctx context.Context, req api.UploadAttachmentRequestObject) (api.UploadAttachmentResponseObject, error) {
	return s.files.UploadAttachment(ctx, req)
}

func (s *server) GetAttachmentLink(ctx context.Context, req api.GetAttachmentLinkRequestObject) (api.GetAttachmentLinkResponseObject, error) {
	return s.files.GetAttachmentLink(ctx, req)
}

func (s *server) ListDevices(ctx context.Context, req api.ListDevicesRequestObject) (api.ListDevicesResponseObject, error) {
	return s.secrets.ListDevices(ctx, req)
}

func (s *server) RegisterDevice(ctx context.Context, req api.RegisterDeviceRequestObject) (api.RegisterDeviceResponseObject, error) {
	return s.secrets.RegisterDevice(ctx, req)
}

func (s *server) RevokeDevice(ctx context.Context, req api.RevokeDeviceRequestObject) (api.RevokeDeviceResponseObject, error) {
	return s.secrets.RevokeDevice(ctx, req)
}

func (s *server) ListUserDevices(ctx context.Context, req api.ListUserDevicesRequestObject) (api.ListUserDevicesResponseObject, error) {
	return s.secrets.ListUserDevices(ctx, req)
}

func (s *server) UploadOneTimeKeys(ctx context.Context, req api.UploadOneTimeKeysRequestObject) (api.UploadOneTimeKeysResponseObject, error) {
	return s.secrets.UploadOneTimeKeys(ctx, req)
}

func (s *server) CountOneTimeKeys(ctx context.Context, req api.CountOneTimeKeysRequestObject) (api.CountOneTimeKeysResponseObject, error) {
	return s.secrets.CountOneTimeKeys(ctx, req)
}

func (s *server) GetKeyBundle(ctx context.Context, req api.GetKeyBundleRequestObject) (api.GetKeyBundleResponseObject, error) {
	return s.secrets.GetKeyBundle(ctx, req)
}

func (s *server) ListSecretChats(ctx context.Context, req api.ListSecretChatsRequestObject) (api.ListSecretChatsResponseObject, error) {
	return s.secrets.ListSecretChats(ctx, req)
}

func (s *server) StartSecretChat(ctx context.Context, req api.StartSecretChatRequestObject) (api.StartSecretChatResponseObject, error) {
	return s.secrets.StartSecretChat(ctx, req)
}

func (s *server) GetSecretChat(ctx context.Context, req api.GetSecretChatRequestObject) (api.GetSecretChatResponseObject, error) {
	return s.secrets.GetSecretChat(ctx, req)
}

func (s *server) LeaveSecretChat(ctx context.Context, req api.LeaveSecretChatRequestObject) (api.LeaveSecretChatResponseObject, error) {
	return s.secrets.LeaveSecretChat(ctx, req)
}

func (s *server) ListSecretEnvelopes(ctx context.Context, req api.ListSecretEnvelopesRequestObject) (api.ListSecretEnvelopesResponseObject, error) {
	return s.secrets.ListSecretEnvelopes(ctx, req)
}

func (s *server) SendSecretEnvelope(ctx context.Context, req api.SendSecretEnvelopeRequestObject) (api.SendSecretEnvelopeResponseObject, error) {
	return s.secrets.SendSecretEnvelope(ctx, req)
}

func (s *server) AcknowledgeSecretEnvelope(ctx context.Context, req api.AcknowledgeSecretEnvelopeRequestObject) (api.AcknowledgeSecretEnvelopeResponseObject, error) {
	return s.secrets.AcknowledgeSecretEnvelope(ctx, req)
}

func (s *server) ListConversations(ctx context.Context, req api.ListConversationsRequestObject) (api.ListConversationsResponseObject, error) {
	return s.conversations.ListConversations(ctx, req)
}

func (s *server) StartConversation(ctx context.Context, req api.StartConversationRequestObject) (api.StartConversationResponseObject, error) {
	return s.conversations.StartConversation(ctx, req)
}

func (s *server) GetConversation(ctx context.Context, req api.GetConversationRequestObject) (api.GetConversationResponseObject, error) {
	return s.conversations.GetConversation(ctx, req)
}

func (s *server) ListUserBlocks(ctx context.Context, req api.ListUserBlocksRequestObject) (api.ListUserBlocksResponseObject, error) {
	return s.privacy.ListUserBlocks(ctx, req)
}

func (s *server) BlockUser(ctx context.Context, req api.BlockUserRequestObject) (api.BlockUserResponseObject, error) {
	return s.privacy.BlockUser(ctx, req)
}

func (s *server) UnblockUser(ctx context.Context, req api.UnblockUserRequestObject) (api.UnblockUserResponseObject, error) {
	return s.privacy.UnblockUser(ctx, req)
}

func (s *server) GetDmPolicy(ctx context.Context, req api.GetDmPolicyRequestObject) (api.GetDmPolicyResponseObject, error) {
	return s.privacy.GetDmPolicy(ctx, req)
}

func (s *server) SetDmPolicy(ctx context.Context, req api.SetDmPolicyRequestObject) (api.SetDmPolicyResponseObject, error) {
	return s.privacy.SetDmPolicy(ctx, req)
}

func (s *server) UpdateMyProfile(ctx context.Context, req api.UpdateMyProfileRequestObject) (api.UpdateMyProfileResponseObject, error) {
	return s.profiles.UpdateMyProfile(ctx, req)
}

func (s *server) SetMyStatus(ctx context.Context, req api.SetMyStatusRequestObject) (api.SetMyStatusResponseObject, error) {
	return s.profiles.SetMyStatus(ctx, req)
}

func (s *server) ClearMyStatus(ctx context.Context, req api.ClearMyStatusRequestObject) (api.ClearMyStatusResponseObject, error) {
	return s.profiles.ClearMyStatus(ctx, req)
}

func (s *server) GetUserProfile(ctx context.Context, req api.GetUserProfileRequestObject) (api.GetUserProfileResponseObject, error) {
	return s.profiles.GetUserProfile(ctx, req)
}

func (s *server) GetMutual(ctx context.Context, req api.GetMutualRequestObject) (api.GetMutualResponseObject, error) {
	return s.profiles.GetMutual(ctx, req)
}

func (s *server) UploadMyAvatar(ctx context.Context, req api.UploadMyAvatarRequestObject) (api.UploadMyAvatarResponseObject, error) {
	return s.profiles.UploadMyAvatar(ctx, req)
}

func (s *server) DeleteMyAvatar(ctx context.Context, req api.DeleteMyAvatarRequestObject) (api.DeleteMyAvatarResponseObject, error) {
	return s.profiles.DeleteMyAvatar(ctx, req)
}

func (s *server) UpdateMyNickname(ctx context.Context, req api.UpdateMyNicknameRequestObject) (api.UpdateMyNicknameResponseObject, error) {
	return s.auth.UpdateMyNickname(ctx, req)
}

func (s *server) UpdateMyEmail(ctx context.Context, req api.UpdateMyEmailRequestObject) (api.UpdateMyEmailResponseObject, error) {
	return s.auth.UpdateMyEmail(ctx, req)
}

func (s *server) GetFriendRequestPolicy(ctx context.Context, req api.GetFriendRequestPolicyRequestObject) (api.GetFriendRequestPolicyResponseObject, error) {
	return s.privacy.GetFriendRequestPolicy(ctx, req)
}

func (s *server) SetFriendRequestPolicy(ctx context.Context, req api.SetFriendRequestPolicyRequestObject) (api.SetFriendRequestPolicyResponseObject, error) {
	return s.privacy.SetFriendRequestPolicy(ctx, req)
}

func (s *server) ListFriends(ctx context.Context, req api.ListFriendsRequestObject) (api.ListFriendsResponseObject, error) {
	return s.friends.ListFriends(ctx, req)
}

func (s *server) CreateFriendRequest(ctx context.Context, req api.CreateFriendRequestRequestObject) (api.CreateFriendRequestResponseObject, error) {
	return s.friends.CreateFriendRequest(ctx, req)
}

func (s *server) ListFriendRequests(ctx context.Context, req api.ListFriendRequestsRequestObject) (api.ListFriendRequestsResponseObject, error) {
	return s.friends.ListFriendRequests(ctx, req)
}

func (s *server) AcceptFriendRequest(ctx context.Context, req api.AcceptFriendRequestRequestObject) (api.AcceptFriendRequestResponseObject, error) {
	return s.friends.AcceptFriendRequest(ctx, req)
}

func (s *server) DeleteFriendRequest(ctx context.Context, req api.DeleteFriendRequestRequestObject) (api.DeleteFriendRequestResponseObject, error) {
	return s.friends.DeleteFriendRequest(ctx, req)
}

func (s *server) RemoveFriend(ctx context.Context, req api.RemoveFriendRequestObject) (api.RemoveFriendResponseObject, error) {
	return s.friends.RemoveFriend(ctx, req)
}

func validationError(_ context.Context, err error, w http.ResponseWriter, _ *http.Request, opts nethttpmiddleware.ErrorHandlerOpts) {
	switch opts.StatusCode {
	case http.StatusNotFound:
		httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "not found")
	case http.StatusMethodNotAllowed:
		httpx.WriteError(w, http.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed, "method not allowed")
	default:
		var reqErr *openapi3filter.RequestError
		msg := err.Error()
		if errors.As(err, &reqErr) {
			msg = reqErr.Error()
		}
		httpx.WriteError(w, http.StatusBadRequest, httpx.CodeInvalidRequest, msg)
	}
}

// limitBody caps the request body. An upload of an attachment, a server icon or
// an avatar gets its own, larger cap: everything else here is small JSON. The
// multipart envelope counts too, so the cap leaves room for it.
func limitBody(normal, upload, icon, avatar int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			limit := normal
			switch {
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/attachments"):
				limit = upload
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/envelopes"):
				// One encrypted message, base64: never more than the envelope cap.
				limit = envelopeBodyBytes
			case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/icon"):
				limit = icon + multipartSlack
			case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/avatar"):
				limit = avatar + multipartSlack
			}
			// A declared size over the limit is refused before a byte is read.
			if r.ContentLength > limit {
				httpx.WriteError(w, http.StatusRequestEntityTooLarge, httpx.CodeBodyTooLarge,
					fmt.Sprintf("request body must not exceed %d bytes", limit))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

func requestLogger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r)
			log.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}
