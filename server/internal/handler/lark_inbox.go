package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/multica-ai/multica/server/internal/integrations/lark"
)

type LarkInboxDestinationResponse struct {
	ID             string `json:"id"`
	InstallationID string `json:"installation_id"`
	ChannelUserID  string `json:"channel_user_id"`
	Status         string `json:"status"`
	Kind           string `json:"kind"`
	AgentID        string `json:"agent_id,omitempty"`
	BotOpenID      string `json:"bot_open_id"`
	Region         string `json:"region"`
	LastTestStatus string `json:"last_test_status,omitempty"`
	LastTestError  string `json:"last_test_error,omitempty"`
	LastTestAt     string `json:"last_test_at,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

type LarkInboxTargetResponse struct {
	InstallationID string `json:"installation_id"`
	Kind           string `json:"kind"`
	AgentID        string `json:"agent_id,omitempty"`
	ChannelUserID  string `json:"channel_user_id"`
	BotOpenID      string `json:"bot_open_id"`
	Region         string `json:"region"`
}

type LarkInboxDeliveryResponse struct {
	ID              string `json:"id"`
	InboxItemID     string `json:"inbox_item_id"`
	InstallationID  string `json:"installation_id"`
	ChannelUserID   string `json:"channel_user_id"`
	Status          string `json:"status"`
	Attempts        int32  `json:"attempts"`
	NextAttemptAt   string `json:"next_attempt_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	CancelledReason string `json:"cancelled_reason,omitempty"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	DeliveredAt     string `json:"delivered_at,omitempty"`
}

type LarkInboxSettingsResponse struct {
	Configured       bool                          `json:"configured"`
	InstallSupported bool                          `json:"install_supported"`
	Destination      *LarkInboxDestinationResponse `json:"destination"`
	Targets          []LarkInboxTargetResponse     `json:"targets"`
	Deliveries       []LarkInboxDeliveryResponse   `json:"deliveries"`
}

func larkInboxSettingsToResponse(settings lark.InboxDestinationSettings, configured, installSupported bool) LarkInboxSettingsResponse {
	resp := LarkInboxSettingsResponse{
		Configured:       configured,
		InstallSupported: installSupported,
		Targets:          make([]LarkInboxTargetResponse, 0, len(settings.Targets)),
		Deliveries:       make([]LarkInboxDeliveryResponse, 0, len(settings.Deliveries)),
	}
	if settings.Destination != nil {
		d := settings.Destination
		resp.Destination = &LarkInboxDestinationResponse{
			ID: d.ID, InstallationID: d.InstallationID, ChannelUserID: d.ChannelUserID,
			Status: d.Status, Kind: d.Kind, AgentID: d.AgentID,
			BotOpenID: d.BotOpenID, Region: d.Region,
			LastTestStatus: d.LastTestStatus, LastTestError: d.LastTestError,
			LastTestAt: formatOptionalTime(d.LastTestAt),
			CreatedAt:  d.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:  d.UpdatedAt.UTC().Format(time.RFC3339),
		}
	}
	for _, target := range settings.Targets {
		resp.Targets = append(resp.Targets, LarkInboxTargetResponse{
			InstallationID: target.InstallationID, Kind: target.Kind,
			AgentID: target.AgentID, ChannelUserID: target.ChannelUserID,
			BotOpenID: target.BotOpenID, Region: target.Region,
		})
	}
	for _, delivery := range settings.Deliveries {
		resp.Deliveries = append(resp.Deliveries, LarkInboxDeliveryResponse{
			ID: delivery.ID, InboxItemID: delivery.InboxItemID,
			InstallationID: delivery.InstallationID, ChannelUserID: delivery.ChannelUserID,
			Status: delivery.Status, Attempts: delivery.Attempts,
			NextAttemptAt: formatOptionalTime(delivery.NextAttemptAt),
			LastError:     delivery.LastError, CancelledReason: delivery.CancelledReason,
			CreatedAt:   delivery.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt:   delivery.UpdatedAt.UTC().Format(time.RFC3339),
			DeliveredAt: formatOptionalTime(delivery.DeliveredAt),
		})
	}
	return resp
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// GetLarkInboxSettings (GET /api/workspaces/{id}/lark/inbox)
// Returns the caller's explicit destination, selectable targets, and recent
// delivery evidence. Never exposes another member's destination.
func (h *Handler) GetLarkInboxSettings(w http.ResponseWriter, r *http.Request) {
	if h.LarkInboxDestinations == nil {
		writeJSON(w, http.StatusOK, LarkInboxSettingsResponse{
			Configured: false, InstallSupported: false,
			Targets: []LarkInboxTargetResponse{}, Deliveries: []LarkInboxDeliveryResponse{},
		})
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	memberUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	settings, err := h.LarkInboxDestinations.Get(r.Context(), wsUUID, memberUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load inbox notification settings")
		return
	}
	writeJSON(w, http.StatusOK, larkInboxSettingsToResponse(settings, true, h.larkInboxInstallSupported()))
}

type SetLarkInboxDestinationRequest struct {
	InstallationID string `json:"installation_id"`
}

// SetLarkInboxDestination (PUT /api/workspaces/{id}/lark/inbox)
func (h *Handler) SetLarkInboxDestination(w http.ResponseWriter, r *http.Request) {
	if h.LarkInboxDestinations == nil {
		writeFeatureDisabled(w, "lark_not_configured", "lark integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	memberUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	var req SetLarkInboxDestinationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	installationUUID, ok := parseUUIDOrBadRequest(w, strings.TrimSpace(req.InstallationID), "installation_id")
	if !ok {
		return
	}
	if _, err := h.LarkInboxDestinations.Set(r.Context(), wsUUID, memberUUID, installationUUID); err != nil {
		switch {
		case errors.Is(err, lark.ErrInboxDestinationUnavailable):
			writeError(w, http.StatusNotFound, "lark installation not found or inactive")
		case errors.Is(err, lark.ErrInboxDestinationForbidden):
			writeError(w, http.StatusForbidden, "this lark bot cannot be used for your inbox")
		case errors.Is(err, lark.ErrInboxDestinationBinding):
			writeError(w, http.StatusBadRequest, "bind your Feishu account to this bot before selecting it")
		default:
			writeError(w, http.StatusInternalServerError, "failed to save inbox notification destination")
		}
		return
	}
	settings, err := h.LarkInboxDestinations.Get(r.Context(), wsUUID, memberUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load inbox notification settings")
		return
	}
	writeJSON(w, http.StatusOK, larkInboxSettingsToResponse(settings, true, h.larkInboxInstallSupported()))
}

// DisableLarkInboxDestination (DELETE /api/workspaces/{id}/lark/inbox)
func (h *Handler) DisableLarkInboxDestination(w http.ResponseWriter, r *http.Request) {
	if h.LarkInboxDestinations == nil {
		writeFeatureDisabled(w, "lark_not_configured", "lark integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	memberUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	if err := h.LarkInboxDestinations.Disable(r.Context(), wsUUID, memberUUID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to disable inbox notification destination")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type LarkInboxTestResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// TestLarkInboxDestination (POST /api/workspaces/{id}/lark/inbox/test)
func (h *Handler) TestLarkInboxDestination(w http.ResponseWriter, r *http.Request) {
	if h.LarkInboxDestinations == nil {
		writeFeatureDisabled(w, "lark_not_configured", "lark integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	memberUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	result, err := h.LarkInboxDestinations.Test(r.Context(), wsUUID, memberUUID)
	if err != nil {
		switch {
		case errors.Is(err, lark.ErrInboxDestinationNotConfigured):
			writeError(w, http.StatusBadRequest, "no inbox notification destination is configured")
		case errors.Is(err, lark.ErrInboxDestinationUnavailable):
			writeError(w, http.StatusConflict, "inbox notification destination is unavailable")
		default:
			writeError(w, http.StatusInternalServerError, "failed to send test notification")
		}
		return
	}
	writeJSON(w, http.StatusOK, LarkInboxTestResponse{OK: result.OK, Error: result.Error})
}

// RetryLarkInboxDelivery (POST /api/workspaces/{id}/lark/inbox/deliveries/{deliveryId}/retry)
func (h *Handler) RetryLarkInboxDelivery(w http.ResponseWriter, r *http.Request) {
	if h.LarkInboxDestinations == nil {
		writeFeatureDisabled(w, "lark_not_configured", "lark integration not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	memberUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	deliveryUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "deliveryId"), "delivery id")
	if !ok {
		return
	}
	if _, err := h.LarkInboxDestinations.Retry(r.Context(), wsUUID, memberUUID, deliveryUUID); err != nil {
		switch {
		case errors.Is(err, lark.ErrInboxDeliveryNotRetryable):
			writeError(w, http.StatusConflict, "delivery is not in a retryable state")
		case errors.Is(err, lark.ErrInboxDeliveryTargetChanged):
			writeError(w, http.StatusConflict, "delivery target is no longer the active destination")
		case errors.Is(err, lark.ErrInboxDestinationNotConfigured):
			writeError(w, http.StatusConflict, "no inbox notification destination is configured")
		case isNotFound(err):
			writeError(w, http.StatusNotFound, "delivery not found")
		default:
			writeError(w, http.StatusInternalServerError, "failed to retry delivery")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// BeginLarkInboxInstall (POST /api/workspaces/{id}/lark/inbox/install/begin)
// Starts the same device flow as an agent bot, but records the resulting
// installation with purpose='inbox' and no agent. The caller becomes the
// installer and is auto-bound by the install service.
func (h *Handler) BeginLarkInboxInstall(w http.ResponseWriter, r *http.Request) {
	if h.LarkRegistration == nil {
		writeFeatureDisabled(w, "lark_not_configured", "lark install not configured")
		return
	}
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	wsUUID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "workspace id")
	if !ok {
		return
	}
	initiatorUUID, ok := parseUUIDOrBadRequest(w, userID, "user id")
	if !ok {
		return
	}
	regionParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("region")))
	switch regionParam {
	case "", "feishu", "lark":
	default:
		writeError(w, http.StatusBadRequest, "region must be 'feishu' or 'lark'")
		return
	}
	res, err := h.LarkRegistration.BeginInstall(r.Context(), lark.BeginInstallParams{
		WorkspaceID: wsUUID,
		Purpose:     lark.InstallationPurposeInbox,
		InitiatorID: initiatorUUID,
		Region:      lark.Region(regionParam),
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to start install: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, BeginLarkInstallResponse{
		SessionID:           res.SessionID,
		QRCodeURL:           res.QRCodeURL,
		ExpiresInSeconds:    res.ExpiresInSeconds,
		PollIntervalSeconds: res.PollIntervalSeconds,
	})
}

func (h *Handler) larkInboxInstallSupported() bool {
	return h.LarkRegistration != nil && h.LarkAPIClient != nil && h.LarkAPIClient.IsConfigured()
}
