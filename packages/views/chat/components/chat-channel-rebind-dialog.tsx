"use client";

import { useState } from "react";
import { Loader2, RefreshCw } from "lucide-react";
import { useAttachChatSessionChannelBinding } from "@multica/core/chat/mutations";
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
import { useT } from "../../i18n";

type RebindError = "conflict" | "forbidden" | "not_found" | "generic";

function errorKey(error: unknown): RebindError {
  if (typeof error === "object" && error !== null && "status" in error) {
    const status = (error as { status?: unknown }).status;
    if (status === 409) return "conflict";
    if (status === 403) return "forbidden";
    if (status === 404) return "not_found";
  }
  return "generic";
}

/**
 * Reattaches this Chat to the Feishu conversation it came from, after archiving
 * retired that route (MP-70). No picker: the conversation is the one recorded on
 * the Chat's own binding, so the only decision is "yes".
 *
 * A Chat whose binding row was deleted by an older version has provenance but no
 * recorded conversation — the ingested messages survive, the route does not. That
 * case gets the manual recovery path instead of a button the server would have to
 * refuse.
 */
export function ChatChannelRebindDialog({ session }: { session: ChatSession }) {
  const { t } = useT("chat");
  const attach = useAttachChatSessionChannelBinding();
  const [open, setOpen] = useState(false);
  const [error, setError] = useState<RebindError | null>(null);

  // channel_source is derived from a binding row in any state, so its presence
  // is exactly "the server still knows which Feishu conversation this was".
  const restorable = session.channel_source != null;

  const errorText = !error
    ? null
    : error === "conflict"
      ? t(($) => $.channel.rebind_dialog.conflict_error)
      : error === "forbidden"
        ? t(($) => $.channel.rebind_dialog.forbidden_error)
        : error === "not_found"
          ? t(($) => $.channel.rebind_dialog.not_found_error)
          : t(($) => $.channel.rebind_dialog.generic_error);

  const handleOpenChange = (next: boolean) => {
    if (attach.isPending) return;
    if (next) setError(null);
    setOpen(next);
  };

  const handleConfirm = async () => {
    setError(null);
    try {
      await attach.mutateAsync({ sessionId: session.id });
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
        {t(($) => $.channel.rebind)}
      </DialogTrigger>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>
            {restorable
              ? t(($) => $.channel.rebind)
              : t(($) => $.channel.rebind_dialog.unavailable_title)}
          </DialogTitle>
          <DialogDescription>
            {restorable
              ? t(($) => $.channel.rebind_dialog.description)
              : t(($) => $.channel.rebind_dialog.unavailable)}
          </DialogDescription>
        </DialogHeader>

        {errorText && (
          <p role="alert" className="text-caption text-destructive">
            {errorText}
          </p>
        )}

        <DialogFooter>
          {restorable ? (
            <>
              <DialogClose render={<Button type="button" variant="outline" />}>
                {t(($) => $.channel.rebind_dialog.cancel)}
              </DialogClose>
              <Button
                type="button"
                onClick={() => void handleConfirm()}
                disabled={attach.isPending}
                aria-busy={attach.isPending}
              >
                {attach.isPending && <Loader2 className="size-4 animate-spin" />}
                {attach.isPending
                  ? t(($) => $.channel.rebind_dialog.rebinding)
                  : t(($) => $.channel.rebind_dialog.confirm)}
              </Button>
            </>
          ) : (
            <DialogClose render={<Button type="button" variant="outline" />}>
              {t(($) => $.channel.rebind_dialog.close)}
            </DialogClose>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
