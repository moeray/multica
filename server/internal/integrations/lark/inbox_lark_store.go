package lark

// Channel-backed persistence for the explicit inbox notification destination
// and its durable delivery outbox. The store deliberately exposes generated
// rows: these records are internal bookkeeping and have no platform-specific
// JSON projection to decode.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func (s *ChannelStore) GetInboxLarkDestination(ctx context.Context, workspaceID, memberID pgtype.UUID) (db.InboxLarkDestination, error) {
	return s.Queries.GetInboxLarkDestination(ctx, db.GetInboxLarkDestinationParams{
		WorkspaceID: workspaceID,
		MemberID:    memberID,
	})
}

func (s *ChannelStore) UpsertInboxLarkDestination(ctx context.Context, workspaceID, memberID, installationID pgtype.UUID, channelUserID string) (db.InboxLarkDestination, error) {
	row, err := s.Queries.UpsertInboxLarkDestination(ctx, db.UpsertInboxLarkDestinationParams{
		WorkspaceID:    workspaceID,
		MemberID:       memberID,
		InstallationID: installationID,
		ChannelUserID:  channelUserID,
	})
	if err != nil {
		return db.InboxLarkDestination{}, err
	}
	return inboxDestinationFromUpsertRow(row), nil
}

func (s *ChannelStore) DisableInboxLarkDestination(ctx context.Context, workspaceID, memberID pgtype.UUID) (db.InboxLarkDestination, error) {
	row, err := s.Queries.DisableInboxLarkDestination(ctx, db.DisableInboxLarkDestinationParams{
		WorkspaceID: workspaceID,
		MemberID:    memberID,
	})
	if err != nil {
		return db.InboxLarkDestination{}, err
	}
	return inboxDestinationFromDisableRow(row), nil
}

func (s *ChannelStore) RecordInboxLarkDestinationTest(ctx context.Context, id pgtype.UUID, status, lastError string) error {
	return s.Queries.RecordInboxLarkDestinationTest(ctx, db.RecordInboxLarkDestinationTestParams{
		ID:     id,
		Status: textOrNull(status),
		Error:  textOrNull(lastError),
	})
}

func (s *ChannelStore) FindChannelBindingForMemberOnInstallation(ctx context.Context, workspaceID, memberID, installationID pgtype.UUID) (db.ChannelUserBinding, error) {
	return s.Queries.FindChannelBindingForMemberOnInstallation(ctx, db.FindChannelBindingForMemberOnInstallationParams{
		WorkspaceID:    workspaceID,
		MulticaUserID:  memberID,
		InstallationID: installationID,
	})
}

func (s *ChannelStore) ListInboxLarkTargets(ctx context.Context, workspaceID, memberID pgtype.UUID) ([]db.ListInboxLarkTargetsRow, error) {
	return s.Queries.ListInboxLarkTargets(ctx, db.ListInboxLarkTargetsParams{
		WorkspaceID: workspaceID,
		MemberID:    memberID,
	})
}

func (s *ChannelStore) CreateInboxLarkDelivery(ctx context.Context, arg db.CreateInboxLarkDeliveryParams) (db.InboxLarkDelivery, error) {
	row, err := s.Queries.CreateInboxLarkDelivery(ctx, arg)
	if err != nil {
		return db.InboxLarkDelivery{}, err
	}
	return inboxDeliveryFromCreateRow(row), nil
}

func (s *ChannelStore) ClaimInboxLarkDelivery(ctx context.Context, id, claimToken pgtype.UUID) (db.InboxLarkDelivery, error) {
	return s.Queries.ClaimInboxLarkDelivery(ctx, db.ClaimInboxLarkDeliveryParams{
		ID:         id,
		ClaimToken: claimToken,
	})
}

func (s *ChannelStore) ClaimNextInboxLarkDelivery(ctx context.Context, claimToken pgtype.UUID) (db.InboxLarkDelivery, error) {
	return s.Queries.ClaimNextInboxLarkDelivery(ctx, claimToken)
}

func (s *ChannelStore) MarkInboxLarkDeliveryDelivered(ctx context.Context, id, claimToken pgtype.UUID) (int64, error) {
	return s.Queries.MarkInboxLarkDeliveryDelivered(ctx, db.MarkInboxLarkDeliveryDeliveredParams{
		ID:         id,
		ClaimToken: claimToken,
	})
}

func (s *ChannelStore) RetryInboxLarkDelivery(ctx context.Context, id, claimToken pgtype.UUID, nextAttemptAt pgtype.Timestamptz, lastError string) (int64, error) {
	return s.Queries.RetryInboxLarkDelivery(ctx, db.RetryInboxLarkDeliveryParams{
		ID:            id,
		ClaimToken:    claimToken,
		NextAttemptAt: nextAttemptAt,
		LastError:     textOrNull(lastError),
	})
}

func (s *ChannelStore) FailInboxLarkDelivery(ctx context.Context, id, claimToken pgtype.UUID, lastError string) (int64, error) {
	return s.Queries.FailInboxLarkDelivery(ctx, db.FailInboxLarkDeliveryParams{
		ID:         id,
		ClaimToken: claimToken,
		LastError:  textOrNull(lastError),
	})
}

func (s *ChannelStore) CancelInboxLarkDelivery(ctx context.Context, id, claimToken pgtype.UUID, reason string) (int64, error) {
	return s.Queries.CancelInboxLarkDelivery(ctx, db.CancelInboxLarkDeliveryParams{
		ID:         id,
		ClaimToken: claimToken,
		Reason:     textOrNull(reason),
	})
}

func (s *ChannelStore) RecoverStaleInboxLarkDeliveries(ctx context.Context) (int64, error) {
	return s.Queries.RecoverStaleInboxLarkDeliveries(ctx)
}

func (s *ChannelStore) CancelPendingInboxLarkDeliveriesForTarget(ctx context.Context, workspaceID, memberID, installationID pgtype.UUID, channelUserID, reason string) (int64, error) {
	return s.Queries.CancelPendingInboxLarkDeliveriesForTarget(ctx, db.CancelPendingInboxLarkDeliveriesForTargetParams{
		WorkspaceID:    workspaceID,
		MemberID:       memberID,
		InstallationID: installationID,
		ChannelUserID:  channelUserID,
		Reason:         textOrNull(reason),
	})
}

func (s *ChannelStore) ListInboxLarkDeliveriesForMember(ctx context.Context, workspaceID, memberID pgtype.UUID, limit int32) ([]db.InboxLarkDelivery, error) {
	return s.Queries.ListInboxLarkDeliveriesForMember(ctx, db.ListInboxLarkDeliveriesForMemberParams{
		WorkspaceID: workspaceID,
		MemberID:    memberID,
		Limit:       limit,
	})
}

func (s *ChannelStore) GetInboxLarkDeliveryForMember(ctx context.Context, id, workspaceID, memberID pgtype.UUID) (db.InboxLarkDelivery, error) {
	return s.Queries.GetInboxLarkDeliveryForMember(ctx, db.GetInboxLarkDeliveryForMemberParams{
		ID:          id,
		WorkspaceID: workspaceID,
		MemberID:    memberID,
	})
}

func (s *ChannelStore) RetryFailedInboxLarkDelivery(ctx context.Context, id, workspaceID, memberID pgtype.UUID) (db.InboxLarkDelivery, error) {
	return s.Queries.RetryFailedInboxLarkDelivery(ctx, db.RetryFailedInboxLarkDeliveryParams{
		ID:          id,
		WorkspaceID: workspaceID,
		MemberID:    memberID,
	})
}

// NewInboxLarkClaimToken mints the fence a worker uses to finish a delivery.
func newInboxLarkClaimToken() pgtype.UUID {
	id, err := uuid.NewRandom()
	if err != nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: [16]byte(id), Valid: true}
}

func inboxDestinationFromUpsertRow(row db.UpsertInboxLarkDestinationRow) db.InboxLarkDestination {
	return db.InboxLarkDestination{
		ID: row.ID, WorkspaceID: row.WorkspaceID, MemberID: row.MemberID,
		InstallationID: row.InstallationID, ChannelUserID: row.ChannelUserID,
		Status: row.Status, LastTestStatus: row.LastTestStatus,
		LastTestError: row.LastTestError, LastTestAt: row.LastTestAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func inboxDestinationFromDisableRow(row db.DisableInboxLarkDestinationRow) db.InboxLarkDestination {
	return db.InboxLarkDestination{
		ID: row.ID, WorkspaceID: row.WorkspaceID, MemberID: row.MemberID,
		InstallationID: row.InstallationID, ChannelUserID: row.ChannelUserID,
		Status: row.Status, LastTestStatus: row.LastTestStatus,
		LastTestError: row.LastTestError, LastTestAt: row.LastTestAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func inboxDeliveryFromCreateRow(row db.CreateInboxLarkDeliveryRow) db.InboxLarkDelivery {
	return db.InboxLarkDelivery{
		ID: row.ID, WorkspaceID: row.WorkspaceID, InboxItemID: row.InboxItemID,
		RecipientMemberID: row.RecipientMemberID, InstallationID: row.InstallationID,
		ChannelUserID: row.ChannelUserID, IdempotencyKey: row.IdempotencyKey,
		Payload: row.Payload, Status: row.Status, Attempts: row.Attempts,
		NextAttemptAt: row.NextAttemptAt, ClaimToken: row.ClaimToken,
		ClaimExpiresAt: row.ClaimExpiresAt, LastError: row.LastError,
		CancelledReason: row.CancelledReason, DeliveredAt: row.DeliveredAt,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}
