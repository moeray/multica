package lark

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type fakeInboxDestinationQueries struct {
	destinations  map[string]db.InboxLarkDestination
	installations map[string]Installation
	bindings      map[string]db.ChannelUserBinding
	targets       []db.ListInboxLarkTargetsRow
	deliveries    map[string]db.InboxLarkDelivery
	testStatus    string
	testError     string
}

func newFakeInboxDestinationQueries() *fakeInboxDestinationQueries {
	return &fakeInboxDestinationQueries{
		destinations:  make(map[string]db.InboxLarkDestination),
		installations: make(map[string]Installation),
		bindings:      make(map[string]db.ChannelUserBinding),
		deliveries:    make(map[string]db.InboxLarkDelivery),
	}
}

func inboxBindKey(workspaceID, memberID, installationID pgtype.UUID) string {
	return uuidString(workspaceID) + "/" + uuidString(memberID) + "/" + uuidString(installationID)
}

func (f *fakeInboxDestinationQueries) GetInboxLarkDestination(_ context.Context, workspaceID, memberID pgtype.UUID) (db.InboxLarkDestination, error) {
	destination, ok := f.destinations[inboxMemberKey(workspaceID, memberID)]
	if !ok {
		return db.InboxLarkDestination{}, pgx.ErrNoRows
	}
	return destination, nil
}

func (f *fakeInboxDestinationQueries) UpsertInboxLarkDestination(_ context.Context, workspaceID, memberID, installationID pgtype.UUID, channelUserID string) (db.InboxLarkDestination, error) {
	var id pgtype.UUID
	_ = id.Scan("6666ffff-6666-6666-6666-666666666666")
	destination := db.InboxLarkDestination{
		ID:             id,
		WorkspaceID:    workspaceID,
		MemberID:       memberID,
		InstallationID: installationID,
		ChannelUserID:  channelUserID,
		Status:         "active",
		CreatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
		UpdatedAt:      pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	f.destinations[inboxMemberKey(workspaceID, memberID)] = destination
	return destination, nil
}

func (f *fakeInboxDestinationQueries) DisableInboxLarkDestination(_ context.Context, workspaceID, memberID pgtype.UUID) (db.InboxLarkDestination, error) {
	key := inboxMemberKey(workspaceID, memberID)
	destination, ok := f.destinations[key]
	if !ok {
		return db.InboxLarkDestination{}, pgx.ErrNoRows
	}
	destination.Status = "disabled"
	f.destinations[key] = destination
	return destination, nil
}

func (f *fakeInboxDestinationQueries) RecordInboxLarkDestinationTest(_ context.Context, id pgtype.UUID, status, lastError string) error {
	f.testStatus = status
	f.testError = lastError
	return nil
}

func (f *fakeInboxDestinationQueries) FindChannelBindingForMemberOnInstallation(_ context.Context, workspaceID, memberID, installationID pgtype.UUID) (db.ChannelUserBinding, error) {
	binding, ok := f.bindings[inboxBindKey(workspaceID, memberID, installationID)]
	if !ok {
		return db.ChannelUserBinding{}, pgx.ErrNoRows
	}
	return binding, nil
}

func (f *fakeInboxDestinationQueries) ListInboxLarkTargets(_ context.Context, _, _ pgtype.UUID) ([]db.ListInboxLarkTargetsRow, error) {
	return f.targets, nil
}

func (f *fakeInboxDestinationQueries) GetLarkInstallation(_ context.Context, id pgtype.UUID) (Installation, error) {
	installation, ok := f.installations[uuidString(id)]
	if !ok {
		return Installation{}, pgx.ErrNoRows
	}
	return installation, nil
}

func (f *fakeInboxDestinationQueries) GetWorkspace(_ context.Context, id pgtype.UUID) (db.Workspace, error) {
	return db.Workspace{ID: id, Slug: "acme"}, nil
}

func (f *fakeInboxDestinationQueries) ListInboxLarkDeliveriesForMember(_ context.Context, _, _ pgtype.UUID, _ int32) ([]db.InboxLarkDelivery, error) {
	out := make([]db.InboxLarkDelivery, 0, len(f.deliveries))
	for _, delivery := range f.deliveries {
		out = append(out, delivery)
	}
	return out, nil
}

func (f *fakeInboxDestinationQueries) GetInboxLarkDeliveryForMember(_ context.Context, id, _, _ pgtype.UUID) (db.InboxLarkDelivery, error) {
	delivery, ok := f.deliveries[uuidString(id)]
	if !ok {
		return db.InboxLarkDelivery{}, pgx.ErrNoRows
	}
	return delivery, nil
}

func (f *fakeInboxDestinationQueries) RetryFailedInboxLarkDelivery(_ context.Context, id, _, _ pgtype.UUID) (db.InboxLarkDelivery, error) {
	delivery, ok := f.deliveries[uuidString(id)]
	if !ok || delivery.Status != "failed" {
		return db.InboxLarkDelivery{}, pgx.ErrNoRows
	}
	delivery.Status = "pending"
	delivery.Attempts = 0
	f.deliveries[uuidString(id)] = delivery
	return delivery, nil
}

func TestInboxDestinationGetEmpty(t *testing.T) {
	q := newFakeInboxDestinationQueries()
	service := NewInboxDestinationService(q, fakeCredentials{}, &fakeAPIClient{}, InboxDestinationConfig{})
	settings, err := service.Get(context.Background(), uuidFromString(t, inboxTestWorkspaceID), uuidFromString(t, inboxTestMemberID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if settings.Destination != nil {
		t.Fatalf("destination = %+v, want nil", settings.Destination)
	}
	if len(settings.Targets) != 0 || len(settings.Deliveries) != 0 {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
}

func TestInboxDestinationSetValidatesOwnershipAndBinding(t *testing.T) {
	ws := uuidFromString(t, inboxTestWorkspaceID)
	member := uuidFromString(t, inboxTestMemberID)
	otherMember := uuidFromString(t, "7777abcd-7777-7777-7777-777777777777")
	instID := uuidFromString(t, inboxTestInstallation)

	t.Run("cross workspace is unavailable", func(t *testing.T) {
		q := newFakeInboxDestinationQueries()
		q.installations[uuidString(instID)] = Installation{
			ID: instID, WorkspaceID: uuidFromString(t, "8888bbbb-8888-8888-8888-888888888888"),
			Purpose: InstallationPurposeAgent, Status: string(InstallationActive),
		}
		service := NewInboxDestinationService(q, fakeCredentials{}, &fakeAPIClient{}, InboxDestinationConfig{})
		if _, err := service.Set(context.Background(), ws, member, instID); !errors.Is(err, ErrInboxDestinationUnavailable) {
			t.Fatalf("error = %v, want unavailable", err)
		}
	})

	t.Run("another member's inbox bot is forbidden", func(t *testing.T) {
		q := newFakeInboxDestinationQueries()
		q.installations[uuidString(instID)] = Installation{
			ID: instID, WorkspaceID: ws, Purpose: InstallationPurposeInbox,
			InstallerUserID: otherMember, Status: string(InstallationActive),
		}
		service := NewInboxDestinationService(q, fakeCredentials{}, &fakeAPIClient{}, InboxDestinationConfig{})
		if _, err := service.Set(context.Background(), ws, member, instID); !errors.Is(err, ErrInboxDestinationForbidden) {
			t.Fatalf("error = %v, want forbidden", err)
		}
	})

	t.Run("missing binding asks the member to bind first", func(t *testing.T) {
		q := newFakeInboxDestinationQueries()
		q.installations[uuidString(instID)] = Installation{
			ID: instID, WorkspaceID: ws, Purpose: InstallationPurposeAgent,
			Status: string(InstallationActive),
		}
		service := NewInboxDestinationService(q, fakeCredentials{}, &fakeAPIClient{}, InboxDestinationConfig{})
		if _, err := service.Set(context.Background(), ws, member, instID); !errors.Is(err, ErrInboxDestinationBinding) {
			t.Fatalf("error = %v, want binding required", err)
		}
	})
}

func TestInboxDestinationSetAcceptsOwnInboxBot(t *testing.T) {
	ws := uuidFromString(t, inboxTestWorkspaceID)
	member := uuidFromString(t, inboxTestMemberID)
	instID := uuidFromString(t, inboxTestInstallation)
	q := newFakeInboxDestinationQueries()
	q.installations[uuidString(instID)] = Installation{
		ID: instID, WorkspaceID: ws, Purpose: InstallationPurposeInbox,
		InstallerUserID: member, Status: string(InstallationActive),
	}
	q.bindings[inboxBindKey(ws, member, instID)] = db.ChannelUserBinding{
		InstallationID: instID, WorkspaceID: ws, MulticaUserID: member,
		ChannelUserID: inboxTestChannelUser,
	}
	service := NewInboxDestinationService(q, fakeCredentials{}, &fakeAPIClient{}, InboxDestinationConfig{})
	destination, err := service.Set(context.Background(), ws, member, instID)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if destination.ChannelUserID != inboxTestChannelUser {
		t.Fatalf("channel user = %q", destination.ChannelUserID)
	}
}

func TestInboxDestinationTestRecordsSuccessAndFailure(t *testing.T) {
	ws := uuidFromString(t, inboxTestWorkspaceID)
	member := uuidFromString(t, inboxTestMemberID)
	instID := uuidFromString(t, inboxTestInstallation)
	q := newFakeInboxDestinationQueries()
	q.installations[uuidString(instID)] = Installation{
		ID: instID, WorkspaceID: ws, Purpose: InstallationPurposeAgent,
		AppID: "cli_test_app", Status: string(InstallationActive),
	}
	q.destinations[inboxMemberKey(ws, member)] = db.InboxLarkDestination{
		ID:          uuidFromString(t, "6666ffff-6666-6666-6666-666666666666"),
		WorkspaceID: ws, MemberID: member, InstallationID: instID,
		ChannelUserID: inboxTestChannelUser, Status: "active",
	}
	q.bindings[inboxBindKey(ws, member, instID)] = db.ChannelUserBinding{
		InstallationID: instID, WorkspaceID: ws, MulticaUserID: member,
		ChannelUserID: inboxTestChannelUser,
	}
	api := &fakeAPIClient{}
	service := NewInboxDestinationService(q, fakeCredentials{secret: "secret"}, api, InboxDestinationConfig{})

	result, err := service.Test(context.Background(), ws, member)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !result.OK || q.testStatus != "success" {
		t.Fatalf("result = %+v, recorded = %q", result, q.testStatus)
	}
	if len(api.userTextSent) != 1 || api.userTextSent[0].OpenID != OpenID(inboxTestChannelUser) {
		t.Fatalf("test send = %+v", api.userTextSent)
	}
	if got := len(api.userTextSent[0].UUID); got > 50 {
		t.Fatalf("test uuid length = %d, want <= 50: %q", got, api.userTextSent[0].UUID)
	}

	api.userTextErr = errors.New("lark down")
	result, err = service.Test(context.Background(), ws, member)
	if err != nil {
		t.Fatalf("Test failure: %v", err)
	}
	if result.OK || !strings.Contains(result.Error, "lark down") {
		t.Fatalf("result = %+v", result)
	}
	if q.testStatus != "failed" || !strings.Contains(q.testError, "lark down") {
		t.Fatalf("recorded test = (%q, %q)", q.testStatus, q.testError)
	}
}

func TestInboxDestinationRetryRequiresActiveMatchingTarget(t *testing.T) {
	ws := uuidFromString(t, inboxTestWorkspaceID)
	member := uuidFromString(t, inboxTestMemberID)
	instID := uuidFromString(t, inboxTestInstallation)
	otherID := uuidFromString(t, inboxTestOtherInstall)
	deliveryID := uuidFromString(t, "7777ffff-7777-7777-7777-777777777777")
	q := newFakeInboxDestinationQueries()
	q.destinations[inboxMemberKey(ws, member)] = db.InboxLarkDestination{
		ID:          uuidFromString(t, "6666ffff-6666-6666-6666-666666666666"),
		WorkspaceID: ws, MemberID: member, InstallationID: instID,
		ChannelUserID: inboxTestChannelUser, Status: "active",
	}
	q.deliveries[uuidString(deliveryID)] = db.InboxLarkDelivery{
		ID: deliveryID, WorkspaceID: ws, RecipientMemberID: member,
		InstallationID: instID, ChannelUserID: inboxTestChannelUser,
		Status: "failed", Attempts: 5,
	}
	service := NewInboxDestinationService(q, fakeCredentials{}, &fakeAPIClient{}, InboxDestinationConfig{})
	if _, err := service.Retry(context.Background(), ws, member, deliveryID); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if delivery := q.deliveries[uuidString(deliveryID)]; delivery.Status != "pending" {
		t.Fatalf("status = %q, want pending", delivery.Status)
	}

	delivery := q.deliveries[uuidString(deliveryID)]
	delivery.Status = "failed"
	q.deliveries[uuidString(deliveryID)] = delivery
	q.destinations[inboxMemberKey(ws, member)] = db.InboxLarkDestination{
		ID:          uuidFromString(t, "6666ffff-6666-6666-6666-666666666666"),
		WorkspaceID: ws, MemberID: member, InstallationID: otherID,
		ChannelUserID: inboxTestOtherUser, Status: "active",
	}
	if _, err := service.Retry(context.Background(), ws, member, deliveryID); !errors.Is(err, ErrInboxDeliveryTargetChanged) {
		t.Fatalf("error = %v, want target changed", err)
	}
}

func TestInboxDestinationGetMarksRevokedInstallationUnavailable(t *testing.T) {
	ws := uuidFromString(t, inboxTestWorkspaceID)
	member := uuidFromString(t, inboxTestMemberID)
	instID := uuidFromString(t, inboxTestInstallation)
	q := newFakeInboxDestinationQueries()
	q.installations[uuidString(instID)] = Installation{
		ID: instID, WorkspaceID: ws, Purpose: InstallationPurposeAgent,
		Status: string(InstallationRevoked),
	}
	q.destinations[inboxMemberKey(ws, member)] = db.InboxLarkDestination{
		ID:          uuidFromString(t, "6666ffff-6666-6666-6666-666666666666"),
		WorkspaceID: ws, MemberID: member, InstallationID: instID,
		ChannelUserID: inboxTestChannelUser, Status: "active",
	}
	service := NewInboxDestinationService(q, fakeCredentials{}, &fakeAPIClient{}, InboxDestinationConfig{})
	settings, err := service.Get(context.Background(), ws, member)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if settings.Destination == nil || settings.Destination.Status != "unavailable" {
		t.Fatalf("destination = %+v, want unavailable", settings.Destination)
	}
}
