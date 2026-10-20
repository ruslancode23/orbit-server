package servers

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"testing"

	"github.com/google/uuid"

	"github.com/ruslancode23/orbit-server/internal/api"
	"github.com/ruslancode23/orbit-server/internal/auth"
	"github.com/ruslancode23/orbit-server/internal/platform/httpx"
)

func newTestHandler(t *testing.T) (*Handler, world) {
	t.Helper()
	w := newWorld(t)
	return NewHandler(w.svc, slog.New(slog.NewTextHandler(io.Discard, nil))), w
}

// sessionFor builds a context that looks like a request from this account.
func sessionFor(ctx context.Context, userID uuid.UUID) context.Context {
	return auth.WithSession(ctx, auth.Session{
		ID:   uuid.New(),
		User: auth.User{ID: userID, Nickname: "someone"},
	})
}

func TestHandlerWithoutSessionIsUnauthorized(t *testing.T) {
	h, w := newTestHandler(t)
	ctx := context.Background()

	t.Run("list", func(t *testing.T) {
		resp, err := h.ListWorkspaces(ctx, api.ListWorkspacesRequestObject{})
		if err != nil {
			t.Fatalf("ListWorkspaces: %v", err)
		}
		if _, ok := resp.(api.ListWorkspaces401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("create", func(t *testing.T) {
		resp, err := h.CreateWorkspace(ctx, api.CreateWorkspaceRequestObject{
			Body: &api.CreateWorkspaceJSONRequestBody{Name: "Команда"},
		})
		if err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
		if _, ok := resp.(api.CreateWorkspace401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})

	t.Run("read one", func(t *testing.T) {
		resp, err := h.GetWorkspace(ctx, api.GetWorkspaceRequestObject{WorkspaceId: w.workspace})
		if err != nil {
			t.Fatalf("GetWorkspace: %v", err)
		}
		if _, ok := resp.(api.GetWorkspace401JSONResponse); !ok {
			t.Errorf("response = %T, want 401", resp)
		}
	})
}

// An outsider gets 404, not 403: the API must not reveal which servers exist.
func TestHandlerHidesServersFromOutsiders(t *testing.T) {
	h, w := newTestHandler(t)
	ctx := sessionFor(context.Background(), w.outsider)

	resp, err := h.GetWorkspace(ctx, api.GetWorkspaceRequestObject{WorkspaceId: w.workspace})
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	notFound, ok := resp.(api.GetWorkspace404JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 404", resp)
	}
	if notFound.Error.Code != httpx.CodeNotFound {
		t.Errorf("code = %q, want %q", notFound.Error.Code, httpx.CodeNotFound)
	}
}

// A member is told plainly that the role is not enough; that is not a secret.
func TestHandlerForbidsRenameForMember(t *testing.T) {
	h, w := newTestHandler(t)
	ctx := sessionFor(context.Background(), w.member)

	name := "Моё имя"
	resp, err := h.UpdateWorkspace(ctx, api.UpdateWorkspaceRequestObject{
		WorkspaceId: w.workspace,
		Body:        &api.UpdateWorkspaceJSONRequestBody{Name: &name},
	})
	if err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	forbidden, ok := resp.(api.UpdateWorkspace403JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 403", resp)
	}
	if forbidden.Error.Code != httpx.CodeForbidden {
		t.Errorf("code = %q, want %q", forbidden.Error.Code, httpx.CodeForbidden)
	}
}

func TestHandlerCreateAndListTheCallersServers(t *testing.T) {
	h, w := newTestHandler(t)
	ctx := sessionFor(context.Background(), w.outsider)

	created, err := h.CreateWorkspace(ctx, api.CreateWorkspaceRequestObject{
		Body: &api.CreateWorkspaceJSONRequestBody{Name: "Моя команда"},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	membership, ok := created.(api.CreateWorkspace201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 201", created)
	}
	if membership.Role != api.WorkspaceRole(RoleOwner) {
		t.Errorf("role = %q, want owner", membership.Role)
	}

	listed, err := h.ListWorkspaces(ctx, api.ListWorkspacesRequestObject{})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	mine, ok := listed.(api.ListWorkspaces200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", listed)
	}
	// Only the server just created: the one from newWorld belongs to other people.
	if len(mine) != 1 || mine[0].Workspace.Id != membership.Workspace.Id {
		t.Errorf("servers = %+v, want only the new one", mine)
	}
}

func TestHandlerBanReturnsTheBanWithItsReason(t *testing.T) {
	h, w := newTestHandler(t)
	ctx := sessionFor(context.Background(), w.owner)
	reason := "спам"

	resp, err := h.BanWorkspaceMember(ctx, api.BanWorkspaceMemberRequestObject{
		WorkspaceId: w.workspace,
		UserId:      w.member,
		Body:        &api.BanWorkspaceMemberJSONRequestBody{Reason: &reason},
	})
	if err != nil {
		t.Fatalf("BanWorkspaceMember: %v", err)
	}
	ban, ok := resp.(api.BanWorkspaceMember200JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 200", resp)
	}
	if ban.Reason == nil || *ban.Reason != reason {
		t.Errorf("reason = %v, want %q", ban.Reason, reason)
	}
	if ban.BannedBy == nil || *ban.BannedBy != w.owner {
		t.Errorf("bannedBy = %v, want %v", ban.BannedBy, w.owner)
	}
}

// multipartIcon is the body a browser sends for <input type=file name=file>.
func multipartIcon(t *testing.T, field string, data []byte) *multipart.Reader {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, "logo.png")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return multipart.NewReader(&body, writer.Boundary())
}

func TestHandlerUploadIcon(t *testing.T) {
	w, _, _ := newProfileWorld(t)
	h := NewHandler(w.svc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	t.Run("a png from an admin", func(t *testing.T) {
		resp, err := h.UploadWorkspaceIcon(sessionFor(context.Background(), w.admin), api.UploadWorkspaceIconRequestObject{
			WorkspaceId: w.workspace, Body: multipartIcon(t, "file", pngBytes(100)),
		})
		if err != nil {
			t.Fatalf("UploadWorkspaceIcon: %v", err)
		}
		ok, isOK := resp.(api.UploadWorkspaceIcon200JSONResponse)
		if !isOK || ok.Workspace.IconUrl == nil {
			t.Fatalf("response = %#v, want 200 with an iconUrl", resp)
		}
	})

	t.Run("html named .png", func(t *testing.T) {
		resp, err := h.UploadWorkspaceIcon(sessionFor(context.Background(), w.owner), api.UploadWorkspaceIconRequestObject{
			WorkspaceId: w.workspace, Body: multipartIcon(t, "file", []byte("<html><script>alert(1)</script>")),
		})
		if err != nil {
			t.Fatalf("UploadWorkspaceIcon: %v", err)
		}
		if _, ok := resp.(api.UploadWorkspaceIcon415JSONResponse); !ok {
			t.Errorf("response = %T, want 415", resp)
		}
	})

	t.Run("too large", func(t *testing.T) {
		resp, err := h.UploadWorkspaceIcon(sessionFor(context.Background(), w.owner), api.UploadWorkspaceIconRequestObject{
			WorkspaceId: w.workspace, Body: multipartIcon(t, "file", pngBytes(iconLimit+1)),
		})
		if err != nil {
			t.Fatalf("UploadWorkspaceIcon: %v", err)
		}
		if _, ok := resp.(api.UploadWorkspaceIcon413JSONResponse); !ok {
			t.Errorf("response = %T, want 413", resp)
		}
	})

	t.Run("a member", func(t *testing.T) {
		resp, err := h.UploadWorkspaceIcon(sessionFor(context.Background(), w.member), api.UploadWorkspaceIconRequestObject{
			WorkspaceId: w.workspace, Body: multipartIcon(t, "file", pngBytes(100)),
		})
		if err != nil {
			t.Fatalf("UploadWorkspaceIcon: %v", err)
		}
		if _, ok := resp.(api.UploadWorkspaceIcon403JSONResponse); !ok {
			t.Errorf("response = %T, want 403", resp)
		}
	})

	t.Run("wrong field", func(t *testing.T) {
		resp, err := h.UploadWorkspaceIcon(sessionFor(context.Background(), w.owner), api.UploadWorkspaceIconRequestObject{
			WorkspaceId: w.workspace, Body: multipartIcon(t, "image", pngBytes(100)),
		})
		if err != nil {
			t.Fatalf("UploadWorkspaceIcon: %v", err)
		}
		if _, ok := resp.(api.UploadWorkspaceIcon400JSONResponse); !ok {
			t.Errorf("response = %T, want 400", resp)
		}
	})
}

func TestHandlerPermissionsRoundTrip(t *testing.T) {
	h, w := newTestHandler(t)

	created, err := h.CreateWorkspace(sessionFor(context.Background(), w.outsider), api.CreateWorkspaceRequestObject{
		Body: &api.CreateWorkspaceJSONRequestBody{
			Name:              "Открытая",
			MemberPermissions: &api.WorkspacePermissionsUpdate{CanInvite: on()},
		},
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	membership, ok := created.(api.CreateWorkspace201JSONResponse)
	if !ok {
		t.Fatalf("response = %T, want 201", created)
	}
	want := api.WorkspacePermissions{CanInvite: true, CanCreateChannels: false, CanAttachFiles: true}
	if membership.Workspace.MemberPermissions != want {
		t.Errorf("created permissions = %+v, want %+v", membership.Workspace.MemberPermissions, want)
	}
	if membership.Workspace.Description != nil || membership.Workspace.IconUrl != nil {
		t.Errorf("description = %v, iconUrl = %v, want both null", membership.Workspace.Description, membership.Workspace.IconUrl)
	}

	resp, err := h.UpdateWorkspace(sessionFor(context.Background(), w.member), api.UpdateWorkspaceRequestObject{
		WorkspaceId: w.workspace,
		Body:        &api.UpdateWorkspaceJSONRequestBody{MemberPermissions: &api.WorkspacePermissionsUpdate{CanInvite: on()}},
	})
	if err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	if _, ok := resp.(api.UpdateWorkspace403JSONResponse); !ok {
		t.Errorf("member: response = %T, want 403", resp)
	}

	resp, err = h.UpdateWorkspace(sessionFor(context.Background(), w.admin), api.UpdateWorkspaceRequestObject{
		WorkspaceId: w.workspace,
		Body:        &api.UpdateWorkspaceJSONRequestBody{MemberPermissions: &api.WorkspacePermissionsUpdate{CanAttachFiles: off()}},
	})
	if err != nil {
		t.Fatalf("UpdateWorkspace: %v", err)
	}
	updated, ok := resp.(api.UpdateWorkspace200JSONResponse)
	if !ok {
		t.Fatalf("admin: response = %T, want 200", resp)
	}
	if updated.Workspace.MemberPermissions != (api.WorkspacePermissions{}) {
		t.Errorf("updated permissions = %+v, want everything off", updated.Workspace.MemberPermissions)
	}
}

type fixedPresence struct {
	voice []api.VoicePresence
}

func (f fixedPresence) ForWorkspace(context.Context, uuid.UUID) []api.VoicePresence {
	return f.voice
}

func TestHandlerReturnsVoicePresence(t *testing.T) {
	h, w := newTestHandler(t)
	h.SetVoicePresence(fixedPresence{voice: []api.VoicePresence{{
		ChannelId:    uuid.New(),
		Participants: []api.VoiceParticipant{{UserId: uuid.New(), Name: "Anna"}},
	}}})

	resp, err := h.GetWorkspace(sessionFor(context.Background(), w.member),
		api.GetWorkspaceRequestObject{WorkspaceId: w.workspace})
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	ok, isOK := resp.(api.GetWorkspace200JSONResponse)
	if !isOK {
		t.Fatalf("response = %T, want 200", resp)
	}
	membership := api.WorkspaceMembership(ok)
	if membership.Voice == nil || len(*membership.Voice) != 1 {
		t.Fatalf("voice = %+v, want one channel", membership.Voice)
	}
	if (*membership.Voice)[0].Participants[0].Name != "Anna" {
		t.Errorf("participants = %+v, want Anna", (*membership.Voice)[0].Participants)
	}
}
