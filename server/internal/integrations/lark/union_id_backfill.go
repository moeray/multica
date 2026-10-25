package lark

import (
	"context"
	"log/slog"
	"time"
)

// UnionIDBackfillStore is the narrow slice of ChannelStore the boot
// backfill needs. Declared as an interface (rather than taking the
// concrete *ChannelStore) so the "log once, never re-probe, retry after
// the permission comes back" behaviour is unit-testable without a
// database.
type UnionIDBackfillStore interface {
	ListActiveLarkInstallations(ctx context.Context) ([]Installation, error)
	SetLarkInstallationBotUnionID(ctx context.Context, arg SetInstallationBotUnionIDParams) error
}

// unionIDUnavailableHint is appended to the single diagnostic line an
// installation gets when its union_id is first found unavailable, so the
// operator who sees it in the log knows the exact fix and how to re-run
// the probe without waiting for another release.
const unionIDUnavailableHint = "grant contact:user.base:readonly (and put the bot in the app's contact scope), publish a new app version, then restart the backend with MULTICA_LARK_UNION_ID_BACKFILL_FORCE=true"

// BackfillBotUnionIDs walks every active lark_installation row whose
// bot_union_id is NULL and calls APIClient.GetBotInfo to capture and
// persist it. This is the migration glue for installations created
// before migration 112 added the column — see MUL-2671 group @-mention
// triage. New installs already write bot_union_id during the device-
// flow finalize, so this is a one-shot bridge, not an ongoing job.
//
// Behavior:
//
//   - The function is best-effort and per-installation idempotent:
//     a transient HTTP / decrypt / DB error on one row does not block
//     subsequent rows or interrupt server startup. Every outcome is
//     logged with the installation id so an operator can audit
//     coverage after a deploy.
//
//   - A union_id that Lark refuses to hand out (contact scope denied —
//     41050; or code=0 with the field withheld) is a STABLE state, not a
//     fault. It is recorded once on the installation as
//     BotUnionIDStatusUnavailable plus the Lark diagnostic, and that row
//     is skipped on later boots: the same doomed contact lookup is not
//     re-issued on every restart, and the operator sees one "unavailable"
//     line per installation instead of one WARN per install per boot.
//     The open_id fallback keeps the install usable in single-bot
//     deployments.
//
//   - forceRetry ignores the negative cache for this pass. It is the
//     operator's re-probe path after the app is republished with contact
//     permissions (MULTICA_LARK_UNION_ID_BACKFILL_FORCE=true); a
//     re-install also clears the marker, and a successful probe clears it
//     in place. Transient failures are never cached, so they retry on the
//     next boot either way.
//
//   - Callers should fire this from a separate goroutine at boot so a
//     slow Lark round-trip cannot block HTTP listener startup. The
//     backfill respects ctx cancellation between rows.
//
// Schedule: the router invokes this once per process at boot. There
// is no reason to repeat it on a sweep cadence — once bot_union_id
// is non-NULL, the decoder uses it directly, and a re-install path
// already overwrites both identifiers via UpsertLarkInstallation.
func BackfillBotUnionIDs(
	ctx context.Context,
	queries UnionIDBackfillStore,
	api APIClient,
	creds CredentialsDecrypter,
	log *slog.Logger,
	forceRetry bool,
) {
	if log == nil {
		log = slog.Default()
	}
	if api == nil || !api.IsConfigured() {
		log.Info("lark backfill: APIClient not configured; skipping union_id backfill")
		return
	}
	rows, err := queries.ListActiveLarkInstallations(ctx)
	if err != nil {
		log.Warn("lark backfill: list installations failed", "err", err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var attempted, filled, unavailable, skipped, missed, errored int
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		if row.BotUnionID.Valid && row.BotUnionID.String != "" {
			continue
		}
		if row.BotUnionIDStatus == BotUnionIDStatusUnavailable && !forceRetry {
			// Known permission gap: already diagnosed once, and the
			// answer will not change until an operator republishes the
			// app. Stay quiet at boot.
			skipped++
			log.Debug("lark backfill: union_id still marked unavailable; skipping re-probe",
				"installation_id", uuidString(row.ID),
				"app_id", row.AppID,
				"reason", row.BotUnionIDReason)
			continue
		}
		alreadyMarked := row.BotUnionIDStatus == BotUnionIDStatusUnavailable
		attempted++
		secret, err := creds.DecryptAppSecret(row)
		if err != nil {
			log.Warn("lark backfill: decrypt app_secret failed",
				"installation_id", uuidString(row.ID),
				"app_id", row.AppID,
				"err", err)
			errored++
			continue
		}
		// Bound the Lark round-trip so a single hung install row
		// does not pin the backfill goroutine. 10s matches the
		// http client's defaultRequestTimeout.
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		info, err := api.GetBotInfo(fetchCtx, InstallationCredentials{
			AppID:     row.AppID,
			AppSecret: secret,
			TenantKey: row.TenantKey.String,
			Region:    RegionOrDefault(row.Region),
		})
		cancel()
		if err != nil {
			// Transient (timeout / 5xx / token): no negative cache, so
			// the next boot tries again.
			log.Warn("lark backfill: GetBotInfo failed",
				"installation_id", uuidString(row.ID),
				"app_id", row.AppID,
				"err", err)
			errored++
			continue
		}
		if info.UnionID == "" && !info.UnionIDUnavailable {
			// The client could not classify the miss (no scope denial, but
			// no value either). Do not cache it as unavailable — that
			// would turn one bad minute into a permanently disabled
			// mention path. Retry on the next boot, as before.
			log.Warn("lark backfill: union_id absent and unclassified; leaving NULL",
				"installation_id", uuidString(row.ID),
				"app_id", row.AppID,
				"bot_open_id", string(info.OpenID))
			missed++
			continue
		}
		if info.UnionID == "" {
			reason := info.UnionIDReason
			if reason == "" {
				reason = "union_id unavailable"
			}
			// Record the marker WITHOUT touching bot_union_id: a row
			// that already carries one (re-probe) must not lose it.
			if err := queries.SetLarkInstallationBotUnionID(ctx, SetInstallationBotUnionIDParams{
				ID:        row.ID,
				Status:    BotUnionIDStatusUnavailable,
				Reason:    reason,
				CheckedAt: now,
			}); err != nil {
				log.Warn("lark backfill: persist union_id unavailable marker failed",
					"installation_id", uuidString(row.ID),
					"err", err)
				errored++
				continue
			}
			unavailable++
			if alreadyMarked {
				// Forced re-probe confirmed the same answer. The
				// installation was already diagnosed; do not repeat it.
				log.Debug("lark backfill: union_id still unavailable after forced re-probe",
					"installation_id", uuidString(row.ID),
					"app_id", row.AppID,
					"reason", reason)
			} else {
				// The ONE diagnostic this installation gets. Info, not
				// WARN: a missing union_id degrades mention
				// disambiguation, it does not break the install.
				log.Info("lark backfill: bot union_id unavailable; open_id fallback stays in effect",
					"installation_id", uuidString(row.ID),
					"app_id", row.AppID,
					"bot_open_id", string(info.OpenID),
					"reason", reason,
					"how_to_fix", unionIDUnavailableHint)
			}
			continue
		}
		if err := queries.SetLarkInstallationBotUnionID(ctx, SetInstallationBotUnionIDParams{
			ID:         row.ID,
			BotUnionID: textOrNull(info.UnionID),
			Status:     "",
			Reason:     "",
			CheckedAt:  now,
		}); err != nil {
			log.Warn("lark backfill: persist union_id failed",
				"installation_id", uuidString(row.ID),
				"err", err)
			errored++
			continue
		}
		filled++
		log.Info("lark backfill: stamped union_id",
			"installation_id", uuidString(row.ID),
			"app_id", row.AppID,
			"bot_open_id", string(info.OpenID))
	}
	log.Info("lark backfill: union_id pass complete",
		"attempted", attempted,
		"filled", filled,
		"unavailable", unavailable,
		"skipped", skipped,
		"missed", missed,
		"errored", errored)
}

// CredentialsDecrypter is the narrow surface BackfillBotUnionIDs needs
// from InstallationService. Defined here (rather than imported as a
// concrete *InstallationService) so the backfill can be unit-tested
// with a stub that returns canned plaintext without spinning up the
// secretbox machinery.
type CredentialsDecrypter interface {
	DecryptAppSecret(inst Installation) (string, error)
}
