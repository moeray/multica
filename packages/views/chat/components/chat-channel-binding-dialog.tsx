"use client";

import { useEffect, useMemo, useState } from "react";
import { Loader2, RefreshCw } from "lucide-react";
import { useSwitchChatSessionChannelBinding } from "@multica/core/chat/mutations";
import { sortChatSessions } from "@multica/core/chat/queries";
import type { ChatSession } from "@multica/core/types";
import { Button } from "@multica/ui/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@multica/ui/components/ui/dialog";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";

function errorKey(error: unknown): "conflict" | "forbidden" | "not_found" | "generic" {
  if (typeof error === "object" && error !== null && "status" in error) {
    const status = (error as { status?: unknown }).status;
    if (status === 409) return "conflict";
    if (status === 403) return "forbidden";
    if (status === 404) return "not_found";
  }
  return "generic";
}

/**
 * Chooses an existing Chat as the Feishu p2p route. The list intentionally
 * includes active Chats that already have retired channel bindings; only a Chat
 * with another ACTIVE binding is disabled, because the server refuses to steal
 * that route.
 */
export function ChatChannelBindingDialog({
  session,
  sessions,
}: {
  session: ChatSession;
  sessions: ChatSession[];
}) {
  const { t } = useT("chat");
  const switchBinding = useSwitchChatSessionChannelBinding();
  const [open, setOpen] = useState(false);
  const [selectedID, setSelectedID] = useState<string | null>(null);
  const [error, setError] = useState<"conflict" | "forbidden" | "not_found" | "generic" | null>(null);

  const candidates = useMemo(
    () =>
      sortChatSessions(
        sessions.filter(
          (candidate) =>
            candidate.status === "active" && candidate.agent_id === session.agent_id,
        ),
      ),
    [session.agent_id, sessions],
  );

  useEffect(() => {
    if (!open) return;
    setSelectedID(null);
    setError(null);
  }, [open]);

  const installationID = session.channel_source?.installation_id ?? "";
  const channelChatID = session.channel_source?.channel_chat_id ?? "";
  const canSubmit = !!selectedID && !switchBinding.isPending;
  const label = t(($) => $.channel.change_binding);
  const errorText = !error
    ? null
    : error === "conflict"
      ? t(($) => $.channel.binding_dialog.conflict_error)
      : error === "forbidden"
        ? t(($) => $.channel.binding_dialog.forbidden_error)
        : error === "not_found"
          ? t(($) => $.channel.binding_dialog.not_found_error)
          : t(($) => $.channel.binding_dialog.generic_error);

  const handleOpenChange = (next: boolean) => {
    if (switchBinding.isPending) return;
    setOpen(next);
  };

  const handleConfirm = async () => {
    if (!selectedID || !installationID || !channelChatID) return;
    setError(null);
    try {
      await switchBinding.mutateAsync({
        sessionId: selectedID,
        installationId: installationID,
        channelChatId: channelChatID,
      });
      setOpen(false);
    } catch (err) {
      setError(errorKey(err));
    }
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button
            type="button"
            variant="ghost"
            size="xs"
            className="h-5 shrink-0 gap-1 whitespace-nowrap px-1.5 text-micro text-muted-foreground"
          />
        }
      >
        <RefreshCw className="size-3" />
        {label}
      </DialogTrigger>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{label}</DialogTitle>
          <DialogDescription>{t(($) => $.channel.binding_dialog.description)}</DialogDescription>
        </DialogHeader>

        <div
          role="group"
          aria-label={t(($) => $.channel.binding_dialog.list_aria)}
          className="max-h-[45vh] space-y-1 overflow-y-auto py-1"
        >
          {candidates.map((candidate) => {
            const isCurrent = candidate.id === session.id;
            const hasConflict =
              !isCurrent && candidate.is_current_channel_route === true;
            const disabled = isCurrent || hasConflict;
            const selected = candidate.id === selectedID;
            const title = candidate.title?.trim() || t(($) => $.window.untitled);

            return (
              <button
                key={candidate.id}
                type="button"
                aria-pressed={selected}
                aria-current={isCurrent ? "true" : undefined}
                disabled={disabled}
                onClick={() => setSelectedID(candidate.id)}
                className={cn(
                  "flex w-full items-center gap-3 rounded-md border px-3 py-2 text-left outline-none transition-colors",
                  "focus-visible:ring-1 focus-visible:ring-ring",
                  selected
                    ? "border-brand/40 bg-brand/5"
                    : "border-transparent hover:bg-accent/50",
                  disabled && "cursor-not-allowed opacity-70",
                )}
              >
                <span
                  aria-hidden="true"
                  className={cn(
                    "size-2 shrink-0 rounded-full",
                    selected ? "bg-brand" : "bg-border",
                  )}
                />
                <span className="min-w-0 flex-1 truncate text-body font-medium">{title}</span>
                {isCurrent && (
                  <span className="shrink-0 rounded-full border border-emerald-500/25 bg-emerald-500/10 px-1.5 py-0.5 text-micro text-emerald-700 dark:text-emerald-300">
                    {t(($) => $.channel.current_binding)}
                  </span>
                )}
                {hasConflict && (
                  <span className="shrink-0 rounded-full border border-border bg-muted/40 px-1.5 py-0.5 text-micro text-muted-foreground">
                    {t(($) => $.channel.binding_dialog.conflict)}
                  </span>
                )}
              </button>
            );
          })}
          {candidates.length === 0 && (
            <p className="px-3 py-6 text-center text-caption text-muted-foreground">
              {t(($) => $.channel.binding_dialog.empty)}
            </p>
          )}
        </div>

        {errorText && (
          <p role="alert" className="text-caption text-destructive">
            {errorText}
          </p>
        )}

        <DialogFooter>
          <DialogClose render={<Button type="button" variant="outline" />}>
            {t(($) => $.channel.binding_dialog.cancel)}
          </DialogClose>
          <Button
            type="button"
            onClick={() => void handleConfirm()}
            disabled={!canSubmit}
            aria-busy={switchBinding.isPending}
          >
            {switchBinding.isPending && <Loader2 className="size-4 animate-spin" />}
            {switchBinding.isPending
              ? t(($) => $.channel.binding_dialog.switching)
              : t(($) => $.channel.binding_dialog.confirm)}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
