package lark

// InboxDestinationService owns the member-facing configuration surface for
// inbox notifications. It validates that a target belongs to the caller and
// that the caller actually holds a Feishu identity on that bot; delivery itself
// lives in the Patcher's outbox worker.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

var (
	ErrInboxDestinationNotConfigured = errors.New("inbox notification destination is not configured")
	ErrInboxDestinationUnavailable   = errors.New("inbox notification destination is unavailable")
	ErrInboxDestinationBinding       = errors.New("bind your Feishu account to this bot first")
	ErrInboxDestinationForbidden     = errors.New("this bot cannot be used for your inbox")
	ErrInboxDeliveryNotRetryable     = errors.New("delivery is not in a retryable state")
	ErrInboxDeliveryTargetChanged    = errors.New("delivery target is no longer the active destination")
)

// InboxDestinationQueries is the narrow persistence seam used by the service
// and implemented by *ChannelStore.
type InboxDestinationQueries interface {
	GetInboxLarkDestination(ctx context.Context, workspaceID, memberID pgtype.UUID) (db.InboxLarkDestination, error)
	UpsertInboxLarkDestination(ctx context.Context, workspaceID, memberID, installationID pgtype.UUID, channelUserID string) (db.InboxLarkDestination, error)
	DisableInboxLarkDestination(ctx context.Context, workspaceID, memberID pgtype.UUID) (db.InboxLarkDestination, error)
	RecordInboxLarkDestinationTest(ctx context.Context, id pgtype.UUID, status, lastError string) error
	FindChannelBindingForMemberOnInstallation(ctx context.Context, workspaceID, memberID, installationID pgtype.UUID) (db.ChannelUserBinding, error)
	ListInboxLarkTargets(ctx context.Context, workspaceID, memberID pgtype.UUID) ([]db.ListInboxLarkTargetsRow, error)
	GetLarkInstallation(ctx context.Context, id pgtype.UUID) (Installation, error)
	GetWorkspace(ctx context.Context, id pgtype.UUID) (db.Workspace, error)
	ListInboxLarkDeliveriesForMember(ctx context.Context, workspaceID, memberID pgtype.UUID, limit int32) ([]db.InboxLarkDelivery, error)
	GetInboxLarkDeliveryForMember(ctx context.Context, id, workspaceID, memberID pgtype.UUID) (db.InboxLarkDelivery, error)
	RetryFailedInboxLarkDelivery(ctx context.Context, id, workspaceID, memberID pgtype.UUID) (db.InboxLarkDelivery, error)
}

type InboxDestinationConfig struct {
	Logger *slog.Logger
	Now    func() time.Time
}

func (c InboxDestinationConfig) withDefaults() InboxDestinationConfig {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

type InboxDestinationService struct {
	queries     InboxDestinationQueries
	credentials CredentialsResolver
	client      APIClient
	cfg         InboxDestinationConfig
}

func NewInboxDestinationService(queries InboxDestinationQueries, credentials CredentialsResolver, client APIClient, cfg InboxDestinationConfig) *InboxDestinationService {
	return &InboxDestinationService{
		queries:     queries,
		credentials: credentials,
		client:      client,
		cfg:         cfg.withDefaults(),
	}
}

// InboxDestinationView is the effective state of the saved row. `status` is
// derived: a disabled row stays visible, and a revoked/missing installation
// reads as unavailable instead of pretending delivery still works.
type InboxDestinationView struct {
	ID             string
	InstallationID string
	ChannelUserID  string
	Status         string
	Kind           string
	AgentID        string
	BotOpenID      string
	Region         string
	LastTestStatus string
	LastTestError  string
	LastTestAt     time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type InboxTargetView struct {
	InstallationID string
	Kind           string
	AgentID        string
	ChannelUserID  string
	BotOpenID      string
	Region         string
}

type InboxDeliveryView struct {
	ID              string
	InboxItemID     string
	InstallationID  string
	ChannelUserID   string
	Status          string
	Attempts        int32
	NextAttemptAt   time.Time
	LastError       string
	CancelledReason string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeliveredAt     time.Time
}

type InboxDestinationSettings struct {
	Destination *InboxDestinationView
	Targets     []InboxTargetView
	Deliveries  []InboxDeliveryView
}

type InboxTestResult struct {
	OK    bool
	Error string
}

func (s *InboxDestinationService) Get(ctx context.Context, workspaceID, memberID pgtype.UUID) (InboxDestinationSettings, error) {
	settings := InboxDestinationSettings{Targets: []InboxTargetView{}, Deliveries: []InboxDeliveryView{}}

	targets, err := s.queries.ListInboxLarkTargets(ctx, workspaceID, memberID)
	if err != nil {
		return settings, fmt.Errorf("list inbox lark targets: %w", err)
	}
	for _, target := range targets {
		settings.Targets = append(settings.Targets, InboxTargetView{
			InstallationID: uuidString(target.InstallationID),
			Kind:           purposeOrDefault(target.Purpose),
			AgentID:        uuidString(target.AgentID),
			ChannelUserID:  target.ChannelUserID,
			BotOpenID:      target.BotOpenID,
			Region:         target.Region,
		})
	}

	destination, err := s.queries.GetInboxLarkDestination(ctx, workspaceID, memberID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return settings, fmt.Errorf("load inbox lark destination: %w", err)
		}
	} else {
		view := inboxDestinationView(destination, "unavailable", Installation{})
		inst, instErr := s.queries.GetLarkInstallation(ctx, destination.InstallationID)
		if instErr == nil {
			view = inboxDestinationView(destination, effectiveDestinationStatus(destination.Status, inst), inst)
		}
		settings.Destination = &view
	}

	deliveries, err := s.queries.ListInboxLarkDeliveriesForMember(ctx, workspaceID, memberID, 10)
	if err != nil {
		return settings, fmt.Errorf("list inbox lark deliveries: %w", err)
	}
	for _, delivery := range deliveries {
		settings.Deliveries = append(settings.Deliveries, inboxDeliveryView(delivery))
	}
	return settings, nil
}

func (s *InboxDestinationService) Set(ctx context.Context, workspaceID, memberID, installationID pgtype.UUID) (db.InboxLarkDestination, error) {
	inst, err := s.queries.GetLarkInstallation(ctx, installationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.InboxLarkDestination{}, ErrInboxDestinationUnavailable
		}
		return db.InboxLarkDestination{}, fmt.Errorf("load installation: %w", err)
	}
	if inst.WorkspaceID != workspaceID || inst.Status != string(InstallationActive) {
		return db.InboxLarkDestination{}, ErrInboxDestinationUnavailable
	}
	if purposeOrDefault(inst.Purpose) == InstallationPurposeInbox && inst.InstallerUserID != memberID {
		return db.InboxLarkDestination{}, ErrInboxDestinationForbidden
	}

	binding, err := s.queries.FindChannelBindingForMemberOnInstallation(ctx, workspaceID, memberID, installationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.InboxLarkDestination{}, ErrInboxDestinationBinding
		}
		return db.InboxLarkDestination{}, fmt.Errorf("load member binding: %w", err)
	}
	if strings.TrimSpace(binding.ChannelUserID) == "" {
		return db.InboxLarkDestination{}, ErrInboxDestinationBinding
	}
	return s.queries.UpsertInboxLarkDestination(ctx, workspaceID, memberID, installationID, binding.ChannelUserID)
}

func (s *InboxDestinationService) Disable(ctx context.Context, workspaceID, memberID pgtype.UUID) error {
	_, err := s.queries.DisableInboxLarkDestination(ctx, workspaceID, memberID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("disable inbox lark destination: %w", err)
	}
	return nil
}

func (s *InboxDestinationService) Test(ctx context.Context, workspaceID, memberID pgtype.UUID) (InboxTestResult, error) {
	destination, err := s.queries.GetInboxLarkDestination(ctx, workspaceID, memberID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return InboxTestResult{}, ErrInboxDestinationNotConfigured
		}
		return InboxTestResult{}, fmt.Errorf("load destination: %w", err)
	}
	if destination.Status != "active" {
		return InboxTestResult{}, ErrInboxDestinationNotConfigured
	}
	binding, err := s.queries.FindChannelBindingForMemberOnInstallation(ctx, workspaceID, memberID, destination.InstallationID)
	if err != nil || binding.ChannelUserID != destination.ChannelUserID {
		return InboxTestResult{}, ErrInboxDestinationBinding
	}
	inst, err := s.queries.GetLarkInstallation(ctx, destination.InstallationID)
	if err != nil || inst.Status != string(InstallationActive) || inst.WorkspaceID != workspaceID {
		return InboxTestResult{}, ErrInboxDestinationUnavailable
	}
	if s.client == nil || s.credentials == nil {
		return InboxTestResult{}, ErrInboxDestinationUnavailable
	}
	creds, err := s.credentials.DecryptAppSecret(inst)
	if err != nil {
		_ = s.queries.RecordInboxLarkDestinationTest(ctx, destination.ID, "failed", err.Error())
		return InboxTestResult{Error: err.Error()}, nil
	}

	_, sendErr := s.client.SendUserTextMessage(ctx, SendUserTextParams{
		InstallationID: InstallationCredentials{
			AppID: inst.AppID, AppSecret: creds, Region: RegionOrDefault(inst.Region),
		},
		OpenID: OpenID(destination.ChannelUserID),
		Text:   "Multica inbox notification test",
		UUID:   fmt.Sprintf("inbox-test-%s-%d", uuidString(destination.ID), s.cfg.Now().UnixNano()),
	})
	if sendErr != nil {
		_ = s.queries.RecordInboxLarkDestinationTest(ctx, destination.ID, "failed", sendErr.Error())
		return InboxTestResult{Error: sendErr.Error()}, nil
	}
	if err := s.queries.RecordInboxLarkDestinationTest(ctx, destination.ID, "success", ""); err != nil {
		s.cfg.Logger.Warn("lark inbox: record successful test failed", "error", err)
	}
	return InboxTestResult{OK: true}, nil
}

func (s *InboxDestinationService) Retry(ctx context.Context, workspaceID, memberID, deliveryID pgtype.UUID) (db.InboxLarkDelivery, error) {
	delivery, err := s.queries.GetInboxLarkDeliveryForMember(ctx, deliveryID, workspaceID, memberID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.InboxLarkDelivery{}, pgx.ErrNoRows
		}
		return db.InboxLarkDelivery{}, fmt.Errorf("load delivery: %w", err)
	}
	if delivery.Status != "failed" {
		return db.InboxLarkDelivery{}, ErrInboxDeliveryNotRetryable
	}
	destination, err := s.queries.GetInboxLarkDestination(ctx, workspaceID, memberID)
	if err != nil || destination.Status != "active" ||
		destination.InstallationID != delivery.InstallationID ||
		destination.ChannelUserID != delivery.ChannelUserID {
		return db.InboxLarkDelivery{}, ErrInboxDeliveryTargetChanged
	}
	retried, err := s.queries.RetryFailedInboxLarkDelivery(ctx, deliveryID, workspaceID, memberID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.InboxLarkDelivery{}, ErrInboxDeliveryNotRetryable
		}
		return db.InboxLarkDelivery{}, fmt.Errorf("retry delivery: %w", err)
	}
	return retried, nil
}

func effectiveDestinationStatus(destinationStatus string, inst Installation) string {
	if destinationStatus != "active" {
		return "disabled"
	}
	if inst.Status != string(InstallationActive) {
		return "unavailable"
	}
	return "active"
}

func inboxDestinationView(destination db.InboxLarkDestination, status string, inst Installation) InboxDestinationView {
	view := InboxDestinationView{
		ID:             uuidString(destination.ID),
		InstallationID: uuidString(destination.InstallationID),
		ChannelUserID:  destination.ChannelUserID,
		Status:         status,
		Kind:           purposeOrDefault(inst.Purpose),
		AgentID:        uuidString(inst.AgentID),
		BotOpenID:      inst.BotOpenID,
		Region:         string(RegionOrDefault(inst.Region)),
		LastTestStatus: destination.LastTestStatus.String,
		LastTestError:  destination.LastTestError.String,
		CreatedAt:      destination.CreatedAt.Time,
		UpdatedAt:      destination.UpdatedAt.Time,
	}
	if destination.LastTestAt.Valid {
		view.LastTestAt = destination.LastTestAt.Time
	}
	return view
}

func inboxDeliveryView(delivery db.InboxLarkDelivery) InboxDeliveryView {
	view := InboxDeliveryView{
		ID:              uuidString(delivery.ID),
		InboxItemID:     uuidString(delivery.InboxItemID),
		InstallationID:  uuidString(delivery.InstallationID),
		ChannelUserID:   delivery.ChannelUserID,
		Status:          delivery.Status,
		Attempts:        delivery.Attempts,
		NextAttemptAt:   delivery.NextAttemptAt.Time,
		LastError:       delivery.LastError.String,
		CancelledReason: delivery.CancelledReason.String,
		CreatedAt:       delivery.CreatedAt.Time,
		UpdatedAt:       delivery.UpdatedAt.Time,
	}
	if delivery.DeliveredAt.Valid {
		view.DeliveredAt = delivery.DeliveredAt.Time
	}
	return view
}
