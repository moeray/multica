"use client";

import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { QRCode } from "react-qr-code";
import {
  AlertTriangle,
  BellRing,
  Check,
  Loader2,
  Send,
  Unplug,
} from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@multica/ui/components/ui/dialog";
import { useWorkspaceId } from "@multica/core/hooks";
import { useActorName } from "@multica/core/workspace/hooks";
import { api, ApiError } from "@multica/core/api";
import { larkInboxSettingsOptions, larkKeys } from "@multica/core/lark";
import type {
  InboxLarkDelivery,
  InboxLarkTarget,
  LarkInboxTestResponse,
} from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

// The inbox is shared by web and desktop, so the notification outlet lives
// here rather than in a platform settings page. The source item never changes:
// this dialog only chooses which Feishu bot mirrors the member's inbox items.

interface InstallState {
  sessionId: string;
  qrCodeUrl: string;
  pollMs: number;
}

export function InboxLarkNotificationDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  // Keeping the body unmounted while closed means no settings query runs on
  // every inbox visit; the dialog fetches only when the user opens it.
  if (!open) return null;
  return <InboxLarkNotificationDialogBody onOpenChange={onOpenChange} />;
}

function InboxLarkNotificationDialogBody({
  onOpenChange,
}: {
  onOpenChange: (open: boolean) => void;
}) {
  const { t } = useT("inbox");
  const wsId = useWorkspaceId();
  const qc = useQueryClient();
  const { data, isLoading } = useQuery({
    ...larkInboxSettingsOptions(wsId),
    enabled: !!wsId,
  });
  const settings = data ?? null;
  const [selected, setSelected] = useState("");
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [disabling, setDisabling] = useState(false);
  const [retryingId, setRetryingId] = useState("");
  const [testResult, setTestResult] = useState<LarkInboxTestResponse | null>(null);
  const [installState, setInstallState] = useState<InstallState | null>(null);
  const [installError, setInstallError] = useState("");

  useEffect(() => {
    if (selected || !settings?.destination) return;
    setSelected(settings.destination.installation_id);
  }, [selected, settings?.destination]);

  useEffect(() => {
    if (!installState) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const poll = async () => {
      try {
        const status = await api.getLarkInstallStatus(wsId, installState.sessionId);
        if (cancelled) return;
        if (status.status === "success") {
          setInstallState(null);
          setSelected(status.installation_id ?? "");
          await qc.invalidateQueries({ queryKey: larkKeys.installations(wsId) });
          await qc.invalidateQueries({ queryKey: larkKeys.inboxSettings(wsId) });
          toast.success(t(($) => $.notifications.install_success));
          return;
        }
        if (status.status === "error") {
          setInstallState(null);
          setInstallError(status.error_message || status.error_reason || t(($) => $.notifications.install_failed));
          return;
        }
        timer = setTimeout(() => void poll(), installState.pollMs);
      } catch (error) {
        if (cancelled) return;
        setInstallState(null);
        setInstallError(error instanceof Error ? error.message : t(($) => $.notifications.install_failed));
      }
    };
    timer = setTimeout(() => void poll(), installState.pollMs);
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [installState, qc, t, wsId]);

  const invalidate = async () => {
    await qc.invalidateQueries({ queryKey: larkKeys.inboxSettings(wsId) });
    await qc.invalidateQueries({ queryKey: larkKeys.installations(wsId) });
  };

  async function handleSave() {
    if (!selected || saving) return;
    setSaving(true);
    try {
      await api.setLarkInboxDestination(wsId, selected);
      await invalidate();
      toast.success(t(($) => $.notifications.saved));
    } catch (error) {
      toast.error(errorMessage(error, t(($) => $.notifications.save_failed)));
    } finally {
      setSaving(false);
    }
  }

  async function handleDisable() {
    if (disabling) return;
    setDisabling(true);
    try {
      await api.disableLarkInboxDestination(wsId);
      await invalidate();
      setTestResult(null);
      toast.success(t(($) => $.notifications.disabled));
    } catch (error) {
      toast.error(errorMessage(error, t(($) => $.notifications.disable_failed)));
    } finally {
      setDisabling(false);
    }
  }

  async function handleTest() {
    if (testing) return;
    setTesting(true);
    setTestResult(null);
    try {
      const result = await api.testLarkInboxDestination(wsId);
      setTestResult(result);
      await qc.invalidateQueries({ queryKey: larkKeys.inboxSettings(wsId) });
      if (result.ok) toast.success(t(($) => $.notifications.test_success));
    } catch (error) {
      const message = errorMessage(error, t(($) => $.notifications.test_failed));
      setTestResult({ ok: false, error: message });
      toast.error(message);
    } finally {
      setTesting(false);
    }
  }

  async function handleRetry(deliveryId: string) {
    if (retryingId) return;
    setRetryingId(deliveryId);
    try {
      await api.retryLarkInboxDelivery(wsId, deliveryId);
      await qc.invalidateQueries({ queryKey: larkKeys.inboxSettings(wsId) });
      toast.success(t(($) => $.notifications.retry_started));
    } catch (error) {
      toast.error(errorMessage(error, t(($) => $.notifications.retry_failed)));
    } finally {
      setRetryingId("");
    }
  }

  async function handleInstall() {
    if (installState) return;
    setInstallError("");
    try {
      const result = await api.beginLarkInboxInstall(wsId, "feishu");
      setInstallState({
        sessionId: result.session_id,
        qrCodeUrl: result.qr_code_url,
        pollMs: Math.max(result.poll_interval_seconds, 1) * 1000,
      });
    } catch (error) {
      setInstallError(errorMessage(error, t(($) => $.notifications.install_failed)));
    }
  }

  const destination = settings?.destination ?? null;
  const hasTargets = (settings?.targets.length ?? 0) > 0;
  const failedDeliveries = (settings?.deliveries ?? []).filter(
    (delivery) => delivery.status === "failed",
  );

  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{t(($) => $.notifications.title)}</DialogTitle>
          <DialogDescription>{t(($) => $.notifications.description)}</DialogDescription>
        </DialogHeader>

        {isLoading ? (
          <p className="text-body text-muted-foreground">{t(($) => $.notifications.loading)}</p>
        ) : !settings?.configured ? (
          <div className="rounded-lg border p-4">
            <p className="text-body font-medium">{t(($) => $.notifications.not_configured_title)}</p>
            <p className="mt-1 text-caption text-muted-foreground">
              {t(($) => $.notifications.not_configured_description)}
            </p>
          </div>
        ) : (
          <div className="space-y-5">
            <section className="space-y-2">
              <h3 className="text-body font-medium">{t(($) => $.notifications.current_title)}</h3>
              {destination ? (
                <div className="rounded-lg border p-3">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0 space-y-1">
                      <p className="flex items-center gap-2 text-body font-medium">
                        <BellRing className="size-4 shrink-0 text-muted-foreground" />
                        <TargetName target={destination} />
                      </p>
                      <p className="text-caption text-muted-foreground">
                        {t(($) => $.notifications.channel_user, {
                          id: maskChannelUser(destination.channel_user_id),
                        })}
                      </p>
                    </div>
                    <span
                      className={cn(
                        "shrink-0 rounded-xs bg-muted px-1.5 py-0.5 text-micro",
                        destination.status === "active"
                          ? "text-muted-foreground"
                          : "text-destructive",
                      )}
                    >
                      {destination.status === "active"
                        ? t(($) => $.notifications.status_active)
                        : destination.status === "unavailable"
                          ? t(($) => $.notifications.status_unavailable)
                          : t(($) => $.notifications.status_disabled)}
                    </span>
                  </div>
                  {destination.status !== "active" && (
                    <p className="mt-2 text-caption text-destructive">
                      {destination.status === "unavailable"
                        ? t(($) => $.notifications.status_unavailable_hint)
                        : t(($) => $.notifications.status_disabled_hint)}
                    </p>
                  )}
                  {destination.last_test_status && (
                    <p className="mt-2 text-caption text-muted-foreground">
                      {destination.last_test_status === "success"
                        ? t(($) => $.notifications.last_test_success)
                        : t(($) => $.notifications.last_test_failed, {
                            error: destination.last_test_error ?? "",
                          })}
                    </p>
                  )}
                  {testResult && (
                    <p
                      className={cn(
                        "mt-2 text-caption",
                        testResult.ok ? "text-muted-foreground" : "text-destructive",
                      )}
                    >
                      {testResult.ok
                        ? t(($) => $.notifications.test_success)
                        : t(($) => $.notifications.test_failed_detail, {
                            error: testResult.error ?? "",
                          })}
                    </p>
                  )}
                  <div className="mt-3 flex flex-wrap gap-2">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={handleTest}
                      disabled={testing || destination.status !== "active"}
                    >
                      {testing ? <Loader2 className="size-3 animate-spin" /> : <Send className="size-3" />}
                      {testing ? t(($) => $.notifications.testing) : t(($) => $.notifications.test)}
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={handleDisable}
                      disabled={disabling}
                    >
                      <Unplug className="size-3" />
                      {t(($) => $.notifications.disable)}
                    </Button>
                  </div>
                </div>
              ) : (
                <div className="rounded-lg border border-dashed p-3">
                  <p className="text-body font-medium">{t(($) => $.notifications.none_title)}</p>
                  <p className="mt-1 text-caption text-muted-foreground">
                    {t(($) => $.notifications.none_description)}
                  </p>
                </div>
              )}
            </section>

            <section className="space-y-2">
              <h3 className="text-body font-medium">{t(($) => $.notifications.choose_title)}</h3>
              {hasTargets ? (
                <div className="space-y-1.5" role="radiogroup" aria-label={t(($) => $.notifications.choose_title)}>
                  {(settings?.targets ?? []).map((target) => (
                    <button
                      key={target.installation_id}
                      type="button"
                      role="radio"
                      aria-checked={selected === target.installation_id}
                      onClick={() => setSelected(target.installation_id)}
                      className={cn(
                        "flex w-full items-center justify-between gap-3 rounded-lg border px-3 py-2 text-left transition-colors hover:bg-accent/50",
                        selected === target.installation_id && "border-foreground/30 bg-accent/40",
                      )}
                    >
                      <span className="min-w-0">
                        <span className="block truncate text-body">
                          <TargetName target={target} />
                        </span>
                        <span className="block truncate text-micro text-muted-foreground">
                          {target.kind === "inbox"
                            ? t(($) => $.notifications.kind_inbox)
                            : t(($) => $.notifications.kind_agent)}
                        </span>
                      </span>
                      {selected === target.installation_id && <Check className="size-4 shrink-0" />}
                    </button>
                  ))}
                </div>
              ) : (
                <p className="rounded-lg border border-dashed p-3 text-caption text-muted-foreground">
                  {t(($) => $.notifications.no_targets)}
                </p>
              )}
              <div className="flex flex-wrap gap-2">
                <Button onClick={handleSave} disabled={!selected || saving}>
                  {saving && <Loader2 className="size-3 animate-spin" />}
                  {t(($) => $.notifications.save)}
                </Button>
                {settings?.install_supported && (
                  <Button variant="outline" onClick={handleInstall} disabled={!!installState}>
                    {t(($) => $.notifications.install_inbox_bot)}
                  </Button>
                )}
              </div>
            </section>

            {installState && (
              <section className="space-y-2 rounded-lg border p-3">
                <p className="text-body font-medium">{t(($) => $.notifications.scan_title)}</p>
                <div className="flex justify-center rounded bg-white p-3">
                  <QRCode value={installState.qrCodeUrl} size={148} />
                </div>
                <a
                  href={installState.qrCodeUrl}
                  target="_blank"
                  rel="noreferrer"
                  className="block truncate text-caption text-muted-foreground underline"
                >
                  {installState.qrCodeUrl}
                </a>
                <p className="text-caption text-muted-foreground">
                  {t(($) => $.notifications.scan_description)}
                </p>
              </section>
            )}
            {installError && (
              <p className="flex items-start gap-2 text-caption text-destructive">
                <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                {installError}
              </p>
            )}

            {(settings?.deliveries.length ?? 0) > 0 && (
              <section className="space-y-2">
                <h3 className="text-body font-medium">{t(($) => $.notifications.deliveries_title)}</h3>
                <div className="space-y-1.5">
                  {(settings?.deliveries ?? []).slice(0, 5).map((delivery) => (
                    <DeliveryRow
                      key={delivery.id}
                      delivery={delivery}
                      retrying={retryingId === delivery.id}
                      onRetry={() => void handleRetry(delivery.id)}
                      canRetry={failedDeliveries.some((item) => item.id === delivery.id)}
                    />
                  ))}
                </div>
              </section>
            )}
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {t(($) => $.notifications.close)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function TargetName({ target }: { target: Pick<InboxLarkTarget, "kind" | "agent_id"> }) {
  const { t } = useT("inbox");
  const { getAgentName } = useActorName();
  if (target.kind === "inbox") return <>{t(($) => $.notifications.dedicated_name)}</>;
  return <>{getAgentName(target.agent_id ?? "")}</>;
}

function DeliveryRow({
  delivery,
  retrying,
  canRetry,
  onRetry,
}: {
  delivery: InboxLarkDelivery;
  retrying: boolean;
  canRetry: boolean;
  onRetry: () => void;
}) {
  const { t } = useT("inbox");
  let statusLabel = t(($) => $.notifications.delivery_failed);
  switch (delivery.status) {
    case "pending":
      statusLabel = t(($) => $.notifications.delivery_pending);
      break;
    case "sending":
      statusLabel = t(($) => $.notifications.delivery_sending);
      break;
    case "retry":
      statusLabel = t(($) => $.notifications.delivery_retry);
      break;
    case "delivered":
      statusLabel = t(($) => $.notifications.delivery_delivered);
      break;
    case "cancelled":
      statusLabel = t(($) => $.notifications.delivery_cancelled);
      break;
  }
  return (
    <div className="flex items-start justify-between gap-3 rounded-md border px-3 py-2">
      <div className="min-w-0">
        <p className="text-caption">
          {statusLabel}
          {delivery.attempts > 0 && (
            <span className="ml-1 text-muted-foreground">
              {t(($) => $.notifications.delivery_attempts, { count: delivery.attempts })}
            </span>
          )}
        </p>
        {delivery.last_error && (
          <p className="truncate text-micro text-destructive">{delivery.last_error}</p>
        )}
        {!delivery.last_error && delivery.cancelled_reason && (
          <p className="truncate text-micro text-muted-foreground">{delivery.cancelled_reason}</p>
        )}
      </div>
      {canRetry && (
        <Button variant="ghost" size="sm" onClick={onRetry} disabled={retrying}>
          {retrying && <Loader2 className="size-3 animate-spin" />}
          {t(($) => $.notifications.retry)}
        </Button>
      )}
    </div>
  );
}

function maskChannelUser(value: string): string {
  if (value.length <= 8) return value;
  return `${value.slice(0, 4)}…${value.slice(-4)}`;
}

function errorMessage(error: unknown, fallback: string): string {
  if (error instanceof ApiError) return error.message || fallback;
  if (error instanceof Error) return error.message || fallback;
  return fallback;
}
