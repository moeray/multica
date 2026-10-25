package lark

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// fakeUnionIDBackfillStore is the in-memory UnionIDBackfillStore. Writes
// are applied to the stored rows through the same pure merge the real
// ChannelStore uses, so a second backfill pass observes exactly what the
// database would have held after the first.
type fakeUnionIDBackfillStore struct {
	mu       sync.Mutex
	rows     []Installation
	writes   []SetInstallationBotUnionIDParams
	listErr  error
	writeErr error
}

func (f *fakeUnionIDBackfillStore) ListActiveLarkInstallations(context.Context) ([]Installation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Installation(nil), f.rows...), nil
}

func (f *fakeUnionIDBackfillStore) SetLarkInstallationBotUnionID(_ context.Context, arg SetInstallationBotUnionIDParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, arg)
	for i := range f.rows {
		if f.rows[i].ID == arg.ID {
			f.rows[i] = applyBotUnionIDResolution(f.rows[i], arg)
		}
	}
	return nil
}

func (f *fakeUnionIDBackfillStore) snapshot() ([]Installation, []SetInstallationBotUnionIDParams) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Installation(nil), f.rows...), append([]SetInstallationBotUnionIDParams(nil), f.writes...)
}

// backfillFakeAPI reuses the package's broad fake client (so the rest of
// APIClient stays satisfied) and overrides only GetBotInfo.
type backfillFakeAPI struct {
	*fakeAPIClient
	mu    sync.Mutex
	info  BotInfo
	err   error
	calls int
}

func (f *backfillFakeAPI) GetBotInfo(context.Context, InstallationCredentials) (BotInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.info, f.err
}

func (f *backfillFakeAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type backfillFakeCreds struct{ err error }

func (c backfillFakeCreds) DecryptAppSecret(Installation) (string, error) {
	return "plaintext-secret", c.err
}

func backfillRow(t *testing.T, id, appID string) Installation {
	t.Helper()
	return Installation{
		ID:        uuidFromString(t, id),
		AppID:     appID,
		BotOpenID: "ou_bot_" + appID,
		Region:    string(RegionFeishu),
	}
}

const (
	backfillInstallA = "11111111-1111-1111-1111-111111111111"
	backfillInstallB = "22222222-2222-2222-2222-222222222222"
)

// TestBackfillBotUnionIDs_ScopeDeniedDiagnosedOnce pins the MP-82 startup
// contract: an installation whose contact lookup is refused (41050) is
// recorded ONCE as "union_id unavailable" and never re-probed on later
// boots, so a restart no longer prints a WARN per install.
func TestBackfillBotUnionIDs_ScopeDeniedDiagnosedOnce(t *testing.T) {
	store := &fakeUnionIDBackfillStore{rows: []Installation{backfillRow(t, backfillInstallA, "cli_a")}}
	api := &backfillFakeAPI{
		fakeAPIClient: &fakeAPIClient{},
		info: BotInfo{
			OpenID:             "ou_bot_cli_a",
			UnionIDUnavailable: true,
			UnionIDReason:      `code=41050 msg="no user authority error"`,
		},
	}
	capture, log := newLogCapture()

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)

	rows, writes := store.snapshot()
	if api.callCount() != 1 {
		t.Fatalf("GetBotInfo calls after first pass: got %d want 1", api.callCount())
	}
	if len(writes) != 1 {
		t.Fatalf("writes after first pass: got %d want 1", len(writes))
	}
	if writes[0].Status != BotUnionIDStatusUnavailable {
		t.Errorf("persisted status: got %q want %q", writes[0].Status, BotUnionIDStatusUnavailable)
	}
	if !strings.Contains(writes[0].Reason, "41050") {
		t.Errorf("persisted reason: got %q want the Lark code 41050", writes[0].Reason)
	}
	if writes[0].BotUnionID.Valid {
		t.Errorf("persisted union_id: got %q, want no write on a denied lookup",
			writes[0].BotUnionID.String)
	}
	if rows[0].BotUnionIDStatus != BotUnionIDStatusUnavailable {
		t.Errorf("row status after pass: got %q want %q", rows[0].BotUnionIDStatus, BotUnionIDStatusUnavailable)
	}
	if got := rows[0].BotOpenID; got != "ou_bot_cli_a" {
		t.Errorf("open_id changed: got %q want ou_bot_cli_a (fallback must stay available)", got)
	}

	if n := capture.countAtOrAbove(slog.LevelInfo, "union_id unavailable"); n != 1 {
		t.Errorf("unavailable diagnostics: got %d want exactly 1", n)
	}
	if n := capture.countAtOrAbove(slog.LevelWarn, ""); n != 0 {
		t.Errorf("warnings: got %d want 0 (a denied scope is not a fault)", n)
	}

	// Second boot: the marker suppresses both the request and the log line.
	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)
	if got := api.callCount(); got != 1 {
		t.Errorf("GetBotInfo calls after second pass: got %d want 1 (no re-probe)", got)
	}
	if _, writes := store.snapshot(); len(writes) != 1 {
		t.Errorf("writes after second pass: got %d want 1 (nothing re-persisted)", len(writes))
	}
	if n := capture.countAtOrAbove(slog.LevelInfo, "union_id unavailable"); n != 1 {
		t.Errorf("unavailable diagnostics after second pass: got %d want 1", n)
	}
}

// TestBackfillBotUnionIDs_ForcedRetryAfterPermissionRestored covers the
// operator path: grant the contact permission, republish the app, restart
// with the force switch, and the previously-marked row is stamped with the
// union_id and has its marker cleared.
func TestBackfillBotUnionIDs_ForcedRetryAfterPermissionRestored(t *testing.T) {
	row := backfillRow(t, backfillInstallA, "cli_a")
	row.BotUnionIDStatus = BotUnionIDStatusUnavailable
	row.BotUnionIDReason = `code=41050 msg="no user authority error"`
	row.BotUnionIDCheckedAt = "2026-10-01T00:00:00Z"
	store := &fakeUnionIDBackfillStore{rows: []Installation{row}}
	api := &backfillFakeAPI{
		fakeAPIClient: &fakeAPIClient{},
		info:          BotInfo{OpenID: "ou_bot_cli_a", UnionID: "on_bot_cli_a"},
	}
	capture, log := newLogCapture()

	// Without the switch the marker wins: no request at all.
	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)
	if got := api.callCount(); got != 0 {
		t.Fatalf("GetBotInfo calls without force: got %d want 0", got)
	}

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, true)
	if got := api.callCount(); got != 1 {
		t.Fatalf("GetBotInfo calls with force: got %d want 1", got)
	}
	rows, writes := store.snapshot()
	if len(writes) != 1 {
		t.Fatalf("writes with force: got %d want 1", len(writes))
	}
	if !writes[0].BotUnionID.Valid || writes[0].BotUnionID.String != "on_bot_cli_a" {
		t.Errorf("persisted union_id: got %q (valid=%v) want on_bot_cli_a",
			writes[0].BotUnionID.String, writes[0].BotUnionID.Valid)
	}
	if writes[0].Status != "" {
		t.Errorf("persisted status: got %q want empty (marker cleared)", writes[0].Status)
	}
	if rows[0].BotUnionID.String != "on_bot_cli_a" || rows[0].BotUnionIDStatus != "" {
		t.Errorf("row after retry: union_id=%q status=%q, want on_bot_cli_a / empty",
			rows[0].BotUnionID.String, rows[0].BotUnionIDStatus)
	}
	if n := capture.countAtOrAbove(slog.LevelWarn, ""); n != 0 {
		t.Errorf("warnings: got %d want 0", n)
	}
}

// TestBackfillBotUnionIDs_ForcedRetryTransientFailureClearsMarker pins the
// merged behaviour for an inconclusive forced re-probe: a timeout / 5xx
// must not leave the old "unavailable" marker behind, otherwise the next
// boot WITHOUT the force switch would skip the install again and the
// operator's one retry would be silently undone.
func TestBackfillBotUnionIDs_ForcedRetryTransientFailureClearsMarker(t *testing.T) {
	row := backfillRow(t, backfillInstallA, "cli_a")
	row.BotUnionIDStatus = BotUnionIDStatusUnavailable
	row.BotUnionIDReason = `code=41050 msg="no user authority error"`
	store := &fakeUnionIDBackfillStore{rows: []Installation{row}}
	api := &backfillFakeAPI{
		fakeAPIClient: &fakeAPIClient{},
		err:           errors.New("dial tcp: i/o timeout"),
	}
	capture, log := newLogCapture()

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, true)

	rows, writes := store.snapshot()
	if got := api.callCount(); got != 1 {
		t.Fatalf("GetBotInfo calls with force: got %d want 1", got)
	}
	if len(writes) != 1 {
		t.Fatalf("writes with force: got %d want 1 (marker cleared)", len(writes))
	}
	if writes[0].Status != "" {
		t.Errorf("persisted status: got %q want empty", writes[0].Status)
	}
	if writes[0].BotUnionID.Valid {
		t.Errorf("persisted union_id: got %q, want no write (probe had no value)",
			writes[0].BotUnionID.String)
	}
	if rows[0].BotUnionIDStatus != "" {
		t.Errorf("row status: got %q want empty (next boot must retry)", rows[0].BotUnionIDStatus)
	}
	if n := capture.countAtOrAbove(slog.LevelWarn, "GetBotInfo failed"); n != 1 {
		t.Errorf("warnings: got %d want 1", n)
	}

	// The next boot, without the switch, retries instead of skipping.
	api.mu.Lock()
	api.err = nil
	api.info = BotInfo{OpenID: "ou_bot_cli_a", UnionID: "on_bot_cli_a"}
	api.mu.Unlock()
	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)
	if got := api.callCount(); got != 2 {
		t.Fatalf("GetBotInfo calls after next boot: got %d want 2 (marker cleared, retried)", got)
	}
	rows, _ = store.snapshot()
	if got := rows[0].BotUnionID.String; got != "on_bot_cli_a" {
		t.Errorf("union_id after recovery: got %q want on_bot_cli_a", got)
	}
	if rows[0].BotUnionIDStatus != "" {
		t.Errorf("status after recovery: got %q want empty", rows[0].BotUnionIDStatus)
	}
}

// TestBackfillBotUnionIDs_ForcedRetryUnclassifiedMissClearsMarker covers
// the other inconclusive outcome: GetBotInfo succeeds but reports neither
// a union_id nor a classified reason. It is not a confirmation of the old
// verdict, so the marker must go too.
func TestBackfillBotUnionIDs_ForcedRetryUnclassifiedMissClearsMarker(t *testing.T) {
	row := backfillRow(t, backfillInstallA, "cli_a")
	row.BotUnionIDStatus = BotUnionIDStatusUnavailable
	row.BotUnionIDReason = `code=41050 msg="no user authority error"`
	store := &fakeUnionIDBackfillStore{rows: []Installation{row}}
	api := &backfillFakeAPI{
		fakeAPIClient: &fakeAPIClient{},
		info:          BotInfo{OpenID: "ou_bot_cli_a"},
	}
	_, log := newLogCapture()

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, true)

	rows, writes := store.snapshot()
	if len(writes) != 1 {
		t.Fatalf("writes with force: got %d want 1 (marker cleared)", len(writes))
	}
	if writes[0].Status != "" {
		t.Errorf("persisted status: got %q want empty", writes[0].Status)
	}
	if rows[0].BotUnionIDStatus != "" {
		t.Errorf("row status: got %q want empty", rows[0].BotUnionIDStatus)
	}

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)
	if got := api.callCount(); got != 2 {
		t.Errorf("GetBotInfo calls after next boot: got %d want 2 (retried)", got)
	}
}

// TestBackfillBotUnionIDs_TransientFailureIsNotCached guards the other
// half of the negative cache: a timeout / 5xx is news every boot and must
// NOT mark the installation unavailable, otherwise one bad network minute
// would silently disable mention disambiguation forever.
func TestBackfillBotUnionIDs_TransientFailureIsNotCached(t *testing.T) {
	store := &fakeUnionIDBackfillStore{rows: []Installation{backfillRow(t, backfillInstallA, "cli_a")}}
	api := &backfillFakeAPI{
		fakeAPIClient: &fakeAPIClient{},
		err:           errors.New("dial tcp: i/o timeout"),
	}
	capture, log := newLogCapture()

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)

	rows, writes := store.snapshot()
	if len(writes) != 0 {
		t.Errorf("writes: got %d want 0 (transient failures leave no marker)", len(writes))
	}
	if rows[0].BotUnionIDStatus != "" {
		t.Errorf("row status: got %q want empty", rows[0].BotUnionIDStatus)
	}
	if n := capture.countAtOrAbove(slog.LevelWarn, "GetBotInfo failed"); n != 1 {
		t.Errorf("warnings: got %d want 1", n)
	}

	// The next boot retries it.
	api.mu.Lock()
	api.err = nil
	api.info = BotInfo{OpenID: "ou_bot_cli_a", UnionID: "on_bot_cli_a"}
	api.mu.Unlock()
	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)
	if got := api.callCount(); got != 2 {
		t.Errorf("GetBotInfo calls: got %d want 2 (transient failure retried)", got)
	}
}

// TestBackfillBotUnionIDs_UnclassifiedMissIsNotCached covers the third
// shape: GetBotInfo succeeds but reports neither a union_id nor a
// classified "unavailable" reason (an APIClient that cannot tell). That
// must stay a per-boot WARN with no marker, never a silent permanent
// "unavailable".
func TestBackfillBotUnionIDs_UnclassifiedMissIsNotCached(t *testing.T) {
	store := &fakeUnionIDBackfillStore{rows: []Installation{backfillRow(t, backfillInstallA, "cli_a")}}
	api := &backfillFakeAPI{
		fakeAPIClient: &fakeAPIClient{},
		info:          BotInfo{OpenID: "ou_bot_cli_a"},
	}
	capture, log := newLogCapture()

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)

	if _, writes := store.snapshot(); len(writes) != 0 {
		t.Errorf("writes: got %d want 0", len(writes))
	}
	if n := capture.countAtOrAbove(slog.LevelWarn, "unclassified"); n != 1 {
		t.Errorf("unclassified warnings: got %d want 1", n)
	}

	// Next boot retries instead of skipping.
	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)
	if got := api.callCount(); got != 2 {
		t.Errorf("GetBotInfo calls: got %d want 2", got)
	}
}

// TestBackfillBotUnionIDs_MixedDeployment is the MP-82 startup shape:
// three active installs, one already stamped, one denied, one healthy —
// only the healthy one is fetched and the denied one yields no new
// diagnostic (it was diagnosed on an earlier boot), not a warning storm.
func TestBackfillBotUnionIDs_MixedDeployment(t *testing.T) {
	stamped := backfillRow(t, "33333333-3333-3333-3333-333333333333", "cli_done")
	stamped.BotUnionID = textOrNull("on_already")

	denied := backfillRow(t, backfillInstallA, "cli_denied")
	denied.BotUnionIDStatus = BotUnionIDStatusUnavailable
	denied.BotUnionIDReason = `code=41050 msg="no user authority error"`

	healthy := backfillRow(t, backfillInstallB, "cli_ok")

	store := &fakeUnionIDBackfillStore{rows: []Installation{stamped, denied, healthy}}
	api := &backfillFakeAPI{
		fakeAPIClient: &fakeAPIClient{},
		info:          BotInfo{OpenID: "ou_bot_cli_ok", UnionID: "on_bot_cli_ok"},
	}
	capture, log := newLogCapture()

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)

	if got := api.callCount(); got != 1 {
		t.Errorf("GetBotInfo calls: got %d want 1 (only the healthy install)", got)
	}
	_, writes := store.snapshot()
	if len(writes) != 1 {
		t.Fatalf("writes: got %d want 1", len(writes))
	}
	if !writes[0].BotUnionID.Valid || writes[0].BotUnionID.String != "on_bot_cli_ok" {
		t.Errorf("persisted union_id: got %q want on_bot_cli_ok", writes[0].BotUnionID.String)
	}
	if n := capture.countAtOrAbove(slog.LevelWarn, ""); n != 0 {
		t.Errorf("warnings: got %d want 0", n)
	}
	if n := capture.countAtOrAbove(slog.LevelInfo, "union_id unavailable"); n != 0 {
		t.Errorf("unavailable diagnostics: got %d want 0 (already diagnosed on an earlier boot)", n)
	}
}

// TestBackfillBotUnionIDs_OpenIDFallbackStillRoutes proves the install
// stays usable after a denied lookup: the decoder's open_id fallback,
// which is what runs whenever bot_union_id is empty, still matches the
// bot's own mention.
func TestBackfillBotUnionIDs_OpenIDFallbackStillRoutes(t *testing.T) {
	store := &fakeUnionIDBackfillStore{rows: []Installation{backfillRow(t, backfillInstallA, "cli_a")}}
	api := &backfillFakeAPI{
		fakeAPIClient: &fakeAPIClient{},
		info: BotInfo{
			OpenID:             "ou_bot_cli_a",
			UnionIDUnavailable: true,
			UnionIDReason:      `code=41050 msg="no user authority error"`,
		},
	}
	_, log := newLogCapture()

	BackfillBotUnionIDs(context.Background(), store, api, backfillFakeCreds{}, log, false)

	rows, _ := store.snapshot()
	inst := rows[0]
	var m larkMention
	m.ID.OpenID = inst.BotOpenID
	m.ID.UnionID = "on_someone_elses_bot"
	if !containsMention([]larkMention{m}, inst.BotOpenID, inst.BotUnionID.String) {
		t.Fatal("open_id fallback stopped matching the bot's own mention after a denied union_id lookup")
	}
	if !isBotMention(m, inst.BotOpenID, "") {
		t.Fatal("isBotMention: open_id fallback must match when union_id is empty")
	}
}

// TestApplyBotUnionIDResolution_NeverErasesCapturedUnionID is the
// store-level invariant the backfill depends on: a re-probe that comes
// back unavailable carries no union_id, and applying it must leave a
// previously captured value untouched.
func TestApplyBotUnionIDResolution_NeverErasesCapturedUnionID(t *testing.T) {
	captured := textOrNull("on_captured")
	inst := Installation{BotUnionID: captured}

	got := applyBotUnionIDResolution(inst, SetInstallationBotUnionIDParams{
		Status:    BotUnionIDStatusUnavailable,
		Reason:    `code=41050 msg="no user authority error"`,
		CheckedAt: "2026-10-25T06:00:00Z",
	})
	if got.BotUnionID.String != "on_captured" || !got.BotUnionID.Valid {
		t.Fatalf("union_id erased by a denied re-probe: got %q (valid=%v)", got.BotUnionID.String, got.BotUnionID.Valid)
	}
	if got.BotUnionIDStatus != BotUnionIDStatusUnavailable {
		t.Errorf("status: got %q want %q", got.BotUnionIDStatus, BotUnionIDStatusUnavailable)
	}
	if got.BotUnionIDReason != `code=41050 msg="no user authority error"` {
		t.Errorf("reason: got %q", got.BotUnionIDReason)
	}

	// A successful probe replaces the value AND clears the marker.
	got = applyBotUnionIDResolution(got, SetInstallationBotUnionIDParams{
		BotUnionID: textOrNull("on_fresh"),
		Status:     "",
		Reason:     "",
		CheckedAt:  "2026-10-25T07:00:00Z",
	})
	if got.BotUnionID.String != "on_fresh" {
		t.Errorf("union_id: got %q want on_fresh", got.BotUnionID.String)
	}
	if got.BotUnionIDStatus != "" || got.BotUnionIDReason != "" {
		t.Errorf("marker not cleared: status=%q reason=%q", got.BotUnionIDStatus, got.BotUnionIDReason)
	}
	if got.BotUnionIDCheckedAt != "2026-10-25T07:00:00Z" {
		t.Errorf("checked_at: got %q", got.BotUnionIDCheckedAt)
	}
}

// TestInstallConfig_RoundTripsUnionIDDiagnostics pins that the marker
// actually survives the JSONB round-trip the store performs — otherwise
// the negative cache would be invisible to every subsequent boot.
func TestInstallConfig_RoundTripsUnionIDDiagnostics(t *testing.T) {
	raw, err := encodeInstallConfig(Installation{
		AppID:               "cli_a",
		BotOpenID:           "ou_bot",
		BotUnionIDStatus:    BotUnionIDStatusUnavailable,
		BotUnionIDReason:    `code=41050 msg="no user authority error"`,
		BotUnionIDCheckedAt: "2026-10-25T06:00:00Z",
		Region:              string(RegionFeishu),
	})
	if err != nil {
		t.Fatalf("encodeInstallConfig: %v", err)
	}
	var cfg feishuInstallConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.BotUnionIDStatus != BotUnionIDStatusUnavailable {
		t.Errorf("status round-trip: got %q", cfg.BotUnionIDStatus)
	}
	if cfg.BotUnionIDReason != `code=41050 msg="no user authority error"` {
		t.Errorf("reason round-trip: got %q", cfg.BotUnionIDReason)
	}
	if cfg.BotUnionIDCheckedAt != "2026-10-25T06:00:00Z" {
		t.Errorf("checked_at round-trip: got %q", cfg.BotUnionIDCheckedAt)
	}
	if cfg.BotUnionID != "" {
		t.Errorf("union_id must stay absent: got %q", cfg.BotUnionID)
	}
}
