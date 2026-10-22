package servers

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ruslancode23/orbit-server/internal/perms"
	"github.com/ruslancode23/orbit-server/internal/platform/postgres/db"
	"github.com/ruslancode23/orbit-server/internal/profile"
)

// Pool is the part of pgxpool.Pool this store needs: queries plus transactions,
// because creating a server and banning a member each write two tables at once.
type Pool interface {
	db.DBTX
	Begin(ctx context.Context) (pgx.Tx, error)
}

type PostgresStore struct {
	pool Pool
	q    *db.Queries
}

func NewPostgresStore(pool Pool) *PostgresStore {
	return &PostgresStore{pool: pool, q: db.New(pool)}
}

var _ Store = (*PostgresStore)(nil)

// CreateWorkspace writes the server and its owner membership together: a server
// without an owner would be unmanageable.
func (s *PostgresStore) CreateWorkspace(ctx context.Context, n NewWorkspace, everyone perms.Set, ownerID uuid.UUID) (Workspace, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Workspace{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	row, err := q.CreateWorkspace(ctx, db.CreateWorkspaceParams{
		Name:        n.Name,
		Description: text(n.Description),
		OwnerID:     pgUUID(ownerID),
	})
	if err != nil {
		return Workspace{}, fmt.Errorf("create workspace: %w", err)
	}
	if _, err := q.CreateRole(ctx, db.CreateRoleParams{
		WorkspaceID: row.ID,
		Name:        everyoneName,
		Position:    0,
		Permissions: int64(everyone), //nolint:gosec // perms.Set fits in 13 bits
		IsDefault:   true,
	}); err != nil {
		return Workspace{}, fmt.Errorf("create @everyone: %w", err)
	}
	if _, err := q.AddWorkspaceMember(ctx, db.AddWorkspaceMemberParams{
		WorkspaceID: row.ID,
		UserID:      pgUUID(ownerID),
		Role:        db.WorkspaceRoleOwner,
	}); err != nil {
		return Workspace{}, fmt.Errorf("add owner: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Workspace{}, fmt.Errorf("commit: %w", err)
	}
	return toWorkspace(row, int64(everyone)), nil //nolint:gosec // perms.Set fits in 13 bits
}

func (s *PostgresStore) Workspace(ctx context.Context, id uuid.UUID) (Workspace, error) {
	row, err := s.q.GetWorkspace(ctx, pgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Workspace{}, ErrNotMember
		}
		return Workspace{}, fmt.Errorf("get workspace: %w", err)
	}
	return toWorkspace(row.Workspace, row.EveryonePermissions), nil
}

func (s *PostgresStore) UpdateWorkspace(ctx context.Context, id uuid.UUID, u WorkspaceUpdate) (Workspace, error) {
	params := db.UpdateWorkspaceParams{ID: pgUUID(id)}
	if u.Name != nil {
		params.Name = pgtype.Text{String: *u.Name, Valid: true}
	}
	if u.Description != nil {
		params.SetDescription = true
		params.Description = text(*u.Description)
	}
	if _, err := s.q.UpdateWorkspace(ctx, params); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Workspace{}, ErrNotMember
		}
		return Workspace{}, fmt.Errorf("update workspace: %w", err)
	}
	return s.Workspace(ctx, id)
}

func (s *PostgresStore) SetEveryone(ctx context.Context, workspaceID uuid.UUID, permissions perms.Set) error {
	if err := s.q.SetDefaultRolePermissions(ctx, db.SetDefaultRolePermissionsParams{
		WorkspaceID: pgUUID(workspaceID),
		Permissions: int64(permissions), //nolint:gosec // perms.Set fits in 13 bits
	}); err != nil {
		return fmt.Errorf("set @everyone permissions: %w", err)
	}
	return nil
}

func (s *PostgresStore) SetIcon(ctx context.Context, id uuid.UUID, key string) (string, error) {
	row, err := s.q.SetWorkspaceIcon(ctx, db.SetWorkspaceIconParams{ID: pgUUID(id), IconKey: text(key)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotMember
		}
		return "", fmt.Errorf("set workspace icon: %w", err)
	}
	return row.PreviousIconKey.String, nil
}

func (s *PostgresStore) DeleteWorkspace(ctx context.Context, id uuid.UUID) error {
	if err := s.q.DeleteWorkspace(ctx, pgUUID(id)); err != nil {
		return fmt.Errorf("delete workspace: %w", err)
	}
	return nil
}

func (s *PostgresStore) ListMemberships(ctx context.Context, userID uuid.UUID) ([]Membership, error) {
	rows, err := s.q.ListUserWorkspaces(ctx, pgUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list user workspaces: %w", err)
	}
	out := make([]Membership, 0, len(rows))
	for _, row := range rows {
		base := perms.Set(row.BasePermissions) //nolint:gosec // stored from a perms.Set
		owner := Role(row.Role) == RoleOwner
		if owner {
			base = perms.All
		} else if base.Has(perms.Administrator) {
			base = perms.All
		}
		out = append(out, Membership{
			Workspace:   toWorkspace(row.Workspace, row.EveryonePermissions),
			Role:        roleOf(owner, base),
			Permissions: base,
		})
	}
	return out, nil
}

func (s *PostgresStore) MutualServers(ctx context.Context, callerID, otherID uuid.UUID) ([]SharedServer, error) {
	rows, err := s.q.ListMutualWorkspaces(ctx, db.ListMutualWorkspacesParams{
		UserID:  pgUUID(callerID),
		OtherID: pgUUID(otherID),
	})
	if err != nil {
		return nil, fmt.Errorf("list mutual workspaces: %w", err)
	}
	out := make([]SharedServer, 0, len(rows))
	for _, row := range rows {
		out = append(out, SharedServer{
			ID:      uuid.UUID(row.ID.Bytes),
			Name:    row.Name,
			IconKey: row.IconKey.String,
			Role:    Role(row.Role),
		})
	}
	return out, nil
}

func (s *PostgresStore) Role(ctx context.Context, workspaceID, userID uuid.UUID) (Role, error) {
	role, err := s.q.GetWorkspaceRole(ctx, db.GetWorkspaceRoleParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotMember
		}
		return "", fmt.Errorf("get workspace role: %w", err)
	}
	return Role(role), nil
}

func (s *PostgresStore) Access(ctx context.Context, workspaceID, userID uuid.UUID) (Access, error) {
	role, err := s.Role(ctx, workspaceID, userID)
	if err != nil {
		return Access{}, err
	}
	roleRows, err := s.q.ListWorkspaceRoles(ctx, pgUUID(workspaceID))
	if err != nil {
		return Access{}, fmt.Errorf("list server roles: %w", err)
	}
	held, err := s.q.ListMemberRoleIDs(ctx, db.ListMemberRoleIDsParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
	})
	if err != nil {
		return Access{}, fmt.Errorf("list member roles: %w", err)
	}
	access := Access{Member: perms.Member{Owner: role == RoleOwner}}
	for _, row := range roleRows {
		access.Server.Roles = append(access.Server.Roles, toPermsRole(row))
	}
	for _, id := range held {
		access.Member.Roles = append(access.Member.Roles, uuid.UUID(id.Bytes))
	}
	return access, nil
}

func (s *PostgresStore) GrantAdmin(ctx context.Context, workspaceID, userID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)

	roles, err := q.ListWorkspaceRoles(ctx, pgUUID(workspaceID))
	if err != nil {
		return fmt.Errorf("list server roles: %w", err)
	}
	// The lowest role that grants administrator and nothing else is the
	// server's «Администратор»; roles come highest first.
	var adminRole pgtype.UUID
	for _, role := range roles {
		if !role.IsDefault && role.Permissions == int64(perms.Administrator) {
			adminRole = role.ID
		}
	}
	if !adminRole.Valid {
		top, err := q.MaxRolePosition(ctx, pgUUID(workspaceID))
		if err != nil {
			return fmt.Errorf("top role position: %w", err)
		}
		created, err := q.CreateRole(ctx, db.CreateRoleParams{
			WorkspaceID: pgUUID(workspaceID),
			Name:        adminRoleName,
			Hoist:       true,
			Position:    top + 1,
			Permissions: int64(perms.Administrator),
		})
		if err != nil {
			return fmt.Errorf("create administrator role: %w", err)
		}
		adminRole = created.ID
	}
	if err := q.GrantRole(ctx, db.GrantRoleParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
		RoleID:      adminRole,
	}); err != nil {
		return fmt.Errorf("grant administrator: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *PostgresStore) RevokeAdmin(ctx context.Context, workspaceID, userID uuid.UUID) error {
	if _, err := s.q.RevokeRolesWithPermissions(ctx, db.RevokeRolesWithPermissionsParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
		Bits:        int64(perms.Administrator),
	}); err != nil {
		return fmt.Errorf("revoke administrator: %w", err)
	}
	return nil
}

func (s *PostgresStore) SetRole(ctx context.Context, workspaceID, userID uuid.UUID, role Role) (Member, error) {
	if _, err := s.q.SetWorkspaceRole(ctx, db.SetWorkspaceRoleParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
		Role:        db.WorkspaceRole(role),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Member{}, ErrNotMember
		}
		return Member{}, fmt.Errorf("set workspace role: %w", err)
	}
	return s.member(ctx, workspaceID, userID)
}

func (s *PostgresStore) AddMember(ctx context.Context, workspaceID, userID uuid.UUID, role Role) (Member, error) {
	if _, err := s.q.AddWorkspaceMember(ctx, db.AddWorkspaceMemberParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
		Role:        db.WorkspaceRole(role),
	}); err != nil {
		return Member{}, fmt.Errorf("add workspace member: %w", err)
	}
	return s.member(ctx, workspaceID, userID)
}

func (s *PostgresStore) RemoveMember(ctx context.Context, workspaceID, userID uuid.UUID) error {
	affected, err := s.q.RemoveWorkspaceMember(ctx, db.RemoveWorkspaceMemberParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
	})
	if err != nil {
		return fmt.Errorf("remove workspace member: %w", err)
	}
	if affected == 0 {
		return ErrNotMember
	}
	return nil
}

func (s *PostgresStore) ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]Member, error) {
	rows, err := s.q.ListWorkspaceMembers(ctx, pgUUID(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("list workspace members: %w", err)
	}
	out := make([]Member, 0, len(rows))
	for _, row := range rows {
		owner := Role(row.Role) == RoleOwner
		base := perms.Set(row.BasePermissions) //nolint:gosec // stored from a perms.Set
		if owner || base.Has(perms.Administrator) {
			base = perms.All
		}
		out = append(out, Member{
			// Only the id is known here: Service fills the public part in one batch.
			User:        profile.Public{ID: uuid.UUID(row.UserID.Bytes)},
			Role:        roleOf(owner, base),
			JoinedAt:    row.JoinedAt.Time,
			Permissions: base,
		})
	}
	return out, nil
}

func (s *PostgresStore) CountOwners(ctx context.Context, workspaceID uuid.UUID) (int, error) {
	count, err := s.q.CountWorkspaceOwners(ctx, pgUUID(workspaceID))
	if err != nil {
		return 0, fmt.Errorf("count workspace owners: %w", err)
	}
	return int(count), nil
}

// Ban writes the ban and drops the membership together: half a ban would leave
// the person inside the server.
func (s *PostgresStore) Ban(ctx context.Context, workspaceID, userID, bannedBy uuid.UUID, reason string) (Ban, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ban{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.q.WithTx(tx)
	if _, err := q.BanWorkspaceMember(ctx, db.BanWorkspaceMemberParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
		BannedBy:    pgUUID(bannedBy),
		Reason:      text(reason),
	}); err != nil {
		return Ban{}, fmt.Errorf("ban workspace member: %w", err)
	}
	if _, err := q.RemoveWorkspaceMember(ctx, db.RemoveWorkspaceMemberParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
	}); err != nil {
		return Ban{}, fmt.Errorf("remove banned member: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Ban{}, fmt.Errorf("commit: %w", err)
	}

	bans, err := s.ListBans(ctx, workspaceID)
	if err != nil {
		return Ban{}, err
	}
	for _, ban := range bans {
		if ban.User.ID == userID {
			return ban, nil
		}
	}
	return Ban{}, fmt.Errorf("ban of %s is missing after commit", userID)
}

func (s *PostgresStore) Unban(ctx context.Context, workspaceID, userID uuid.UUID) error {
	affected, err := s.q.UnbanWorkspaceMember(ctx, db.UnbanWorkspaceMemberParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
	})
	if err != nil {
		return fmt.Errorf("unban workspace member: %w", err)
	}
	if affected == 0 {
		return ErrNotBanned
	}
	return nil
}

func (s *PostgresStore) IsBanned(ctx context.Context, workspaceID, userID uuid.UUID) (bool, error) {
	banned, err := s.q.IsWorkspaceMemberBanned(ctx, db.IsWorkspaceMemberBannedParams{
		WorkspaceID: pgUUID(workspaceID),
		UserID:      pgUUID(userID),
	})
	if err != nil {
		return false, fmt.Errorf("is member banned: %w", err)
	}
	return banned, nil
}

func (s *PostgresStore) ListBans(ctx context.Context, workspaceID uuid.UUID) ([]Ban, error) {
	rows, err := s.q.ListWorkspaceBans(ctx, pgUUID(workspaceID))
	if err != nil {
		return nil, fmt.Errorf("list workspace bans: %w", err)
	}
	out := make([]Ban, 0, len(rows))
	for _, row := range rows {
		out = append(out, Ban{
			User:      profile.Public{ID: uuid.UUID(row.UserID.Bytes)},
			Reason:    row.Reason.String,
			BannedBy:  uuid.UUID(row.BannedBy.Bytes),
			CreatedAt: row.CreatedAt.Time,
		})
	}
	return out, nil
}

func (s *PostgresStore) member(ctx context.Context, workspaceID, userID uuid.UUID) (Member, error) {
	members, err := s.ListMembers(ctx, workspaceID)
	if err != nil {
		return Member{}, err
	}
	for _, m := range members {
		if m.User.ID == userID {
			return m, nil
		}
	}
	return Member{}, ErrNotMember
}

// The names the server gives the roles it makes itself.
const (
	everyoneName  = "@everyone"
	adminRoleName = "Администратор"
)

func toWorkspace(row db.Workspace, everyone int64) Workspace {
	return Workspace{
		ID:                uuid.UUID(row.ID.Bytes),
		Name:              row.Name,
		Description:       row.Description.String,
		IconKey:           row.IconKey.String,
		MemberPermissions: permissionsOf(perms.Set(everyone)), //nolint:gosec // stored from a perms.Set
		Everyone:          perms.Set(everyone),                //nolint:gosec // stored from a perms.Set
		OwnerID:           uuid.UUID(row.OwnerID.Bytes),
		CreatedAt:         row.CreatedAt.Time,
	}
}

func toPermsRole(row db.ServerRole) perms.Role {
	return perms.Role{
		ID:          uuid.UUID(row.ID.Bytes),
		Position:    int(row.Position),
		Permissions: perms.Set(row.Permissions), //nolint:gosec // stored from a perms.Set
		Default:     row.IsDefault,
	}
}

func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
