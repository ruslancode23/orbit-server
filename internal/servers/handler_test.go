package servers

import (
	"context"
	"io"
	"log/slog"
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

	resp, err := h.UpdateWorkspace(ctx, api.UpdateWorkspaceRequestObject{
		WorkspaceId: w.workspace,
		Body:        &api.UpdateWorkspaceJSONRequestBody{Name: "Моё имя"},
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
