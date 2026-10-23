"use client";

import type { ChatSession } from "@multica/core/types";
import { cn } from "@multica/ui/lib/utils";
import { useT } from "../../i18n";
import { LarkMark } from "../../settings/components/lark-mark";

type ChannelSession = Pick<ChatSession, "channel_source" | "is_current_channel_route">;

export function isFeishuChatSession(session: ChannelSession): boolean {
  return session.channel_source?.channel_type === "feishu";
}

/**
 * Marks a Chat created from Feishu and distinguishes the active route from a
 * session that was retired by `/new`. Non-channel sessions render nothing.
 */
export function ChatChannelBadge({
  session,
  variant = "list",
  className,
}: {
  session: ChannelSession;
  variant?: "list" | "header";
  className?: string;
}) {
  const { t } = useT("chat");
  if (!isFeishuChatSession(session)) return null;

  const isCurrent = session.is_current_channel_route === true;
  const channel = t(($) => $.channel.feishu);
  const status =
    variant === "header"
      ? isCurrent
        ? t(($) => $.channel.current_binding)
        : t(($) => $.channel.historical_session)
      : isCurrent
        ? t(($) => $.channel.current)
        : t(($) => $.channel.historical);
  const label = `${channel} · ${status}`;

  return (
    <span
      title={label}
      aria-label={label}
      className={cn(
        "inline-flex min-w-0 items-center gap-1 overflow-hidden rounded-full border px-1.5 py-0.5 text-micro font-medium leading-none",
        isCurrent
          ? "border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
          : "border-border bg-muted/40 text-muted-foreground",
        className,
      )}
    >
      <LarkMark className="size-3 shrink-0" />
      <span className="truncate">{channel}</span>
      <span aria-hidden="true" className="shrink-0">
        ·
      </span>
      <span className="truncate">{status}</span>
    </span>
  );
}
