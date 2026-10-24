"use client";

import { useEffect, useRef, useState } from "react";
import { Archive, ArchiveRestore, MoreHorizontal, Pencil, Trash2, UserRound } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from "@multica/ui/components/ui/dropdown-menu";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@multica/ui/components/ui/alert-dialog";
import { useWorkspacePaths } from "@multica/core/paths";
import {
  useUpdateChatSession,
  useDeleteChatSession,
  useSetChatSessionArchived,
} from "@multica/core/chat/mutations";
import { useChatStore } from "@multica/core/chat";
import type { Agent, ChatSession } from "@multica/core/types";
import { isImeComposing } from "@multica/core/utils";
import { ActorAvatar } from "../../common/actor-avatar";
import { AppLink } from "../../navigation";
import { useT } from "../../i18n";
import { ChatChannelBadge, isFeishuChatSession } from "./chat-channel-badge";
import { ChatChannelBindingDialog } from "./chat-channel-binding-dialog";
import { ChatChannelRebindDialog } from "./chat-channel-rebind-dialog";

/**
 * Per-session header for the conversation pane: agent avatar + editable chat
 * title + agent subtitle, with a ⋯ menu (rename / view agent profile / delete).
 * The avatar's hover card is the lightweight "view profile" affordance; the
 * menu item navigates to the full agent page.
 */
export function ChatSessionHeader({
  session,
  agent,
  onArchive,
  sessions = [],
  canChangeBinding = false,
}: {
  session: ChatSession;
  agent: Agent | null;
  // Archiving the open conversation must move the pane off it (advance to the
  // next chat on desktop, back to the list on mobile), so the parent owns it —
  // see ChatPage.handleArchive. Falls back to a plain status flip if unwired.
  onArchive?: (session: ChatSession) => void;
  sessions?: ChatSession[];
  canChangeBinding?: boolean;
}) {
  const { t } = useT("chat");
  const wsPaths = useWorkspacePaths();
  const updateSession = useUpdateChatSession();
  const deleteSession = useDeleteChatSession();
  const setArchived = useSetChatSessionArchived();
  const setActiveSession = useChatStore((s) => s.setActiveSession);

  const isArchived = session.status === "archived";
  const isFeishu = isFeishuChatSession(session);
  const isCurrentChannelRoute = session.is_current_channel_route === true;
  // Channel provenance outlives the binding row: a Chat archived by the older
  // delete-based path keeps only its ingested messages, no channel_source. Both
  // signals qualify, so a Chat broken before the fix still gets the entry. An
  // unknown channel type (that legacy case) is allowed; a known non-Feishu one
  // is not, because this surface only speaks Feishu.
  const hasChannelOrigin = session.channel_origin === true || session.channel_source != null;
  const canRebindChannel =
    canChangeBinding &&
    !isArchived &&
    !isCurrentChannelRoute &&
    hasChannelOrigin &&
    (session.channel_source == null || session.channel_source.channel_type === "feishu");

  const [editing, setEditing] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [confirmArchive, setConfirmArchive] = useState(false);
  const [draft, setDraft] = useState(session.title ?? "");
  const inputRef = useRef<HTMLInputElement>(null);
  const isComposingRef = useRef(false);
  // A browser can blur the input before it emits compositionend. Remember
  // that intent so the final composed value is committed, not the draft.
  const commitAfterCompositionRef = useRef(false);
  const blurCommitTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const title = session.title?.trim() || t(($) => $.window.untitled);

  useEffect(() => {
    if (editing) {
      inputRef.current?.focus();
      inputRef.current?.select();
    }
  }, [editing]);

  useEffect(
    () => () => {
      if (blurCommitTimeoutRef.current !== null) {
        clearTimeout(blurCommitTimeoutRef.current);
      }
    },
    [],
  );

  const clearPendingBlurCommit = () => {
    if (blurCommitTimeoutRef.current === null) return;
    clearTimeout(blurCommitTimeoutRef.current);
    blurCommitTimeoutRef.current = null;
  };

  const startRename = () => {
    clearPendingBlurCommit();
    isComposingRef.current = false;
    commitAfterCompositionRef.current = false;
    setDraft(session.title ?? "");
    setEditing(true);
  };

  const commitRename = (raw = draft) => {
    clearPendingBlurCommit();
    isComposingRef.current = false;
    commitAfterCompositionRef.current = false;
    setEditing(false);
    const trimmed = raw.trim();
    if (!trimmed || trimmed === session.title) return;
    updateSession.mutate({ sessionId: session.id, title: trimmed });
  };

  const doDelete = () => {
    setConfirmDelete(false);
    setActiveSession(null);
    deleteSession.mutate(session.id);
  };

  const archiveNow = () =>
    onArchive
      ? onArchive(session)
      : setArchived.mutate({ sessionId: session.id, archived: true });

  // Archiving is the reversible, normally unconfirmed action. It is only
  // destructive for a Chat that currently receives channel messages, because
  // archiving retires that route: the user has to be told, not surprised by a
  // silently disconnected Feishu chat (MP-70).
  const doArchive = () => {
    if (isCurrentChannelRoute) {
      setConfirmArchive(true);
      return;
    }
    archiveNow();
  };
  const doUnarchive = () => setArchived.mutate({ sessionId: session.id, archived: false });

  return (
    <div className="flex h-12 shrink-0 items-center gap-3 border-b px-4">
      {agent ? (
        <ActorAvatar actorType="agent" actorId={agent.id} size="lg" enableHoverCard showStatusDot />
      ) : (
        <span className="size-[30px] shrink-0" />
      )}

      <div className="min-w-0 flex-1">
        {editing ? (
          <input
            ref={inputRef}
            value={draft}
            maxLength={200}
            aria-label={t(($) => $.header.rename)}
            onChange={(e) => setDraft(e.target.value)}
            onCompositionStart={() => {
              isComposingRef.current = true;
            }}
            onCompositionEnd={(e) => {
              isComposingRef.current = false;
              if (!commitAfterCompositionRef.current) return;
              commitRename(e.currentTarget.value);
            }}
            onBlur={(e) => {
              if (isComposingRef.current) {
                commitAfterCompositionRef.current = true;
                const input = e.currentTarget;
                // Let a compositionend queued by the same focus change win.
                // If it never arrives, commit on the next task instead of
                // leaving an unfocused editor open indefinitely.
                clearPendingBlurCommit();
                blurCommitTimeoutRef.current = setTimeout(() => {
                  if (!commitAfterCompositionRef.current) return;
                  // Without compositionend, the current DOM value is not
                  // known to be final. Close without persisting a partial
                  // composition instead of recreating the original bug.
                  if (isComposingRef.current) {
                    clearPendingBlurCommit();
                    isComposingRef.current = false;
                    commitAfterCompositionRef.current = false;
                    setEditing(false);
                    return;
                  }
                  commitRename(input.value);
                }, 0);
                return;
              }
              commitRename(e.currentTarget.value);
            }}
            onKeyDown={(e) => {
              if (isImeComposing(e)) return;
              if (e.key === "Enter") {
                e.preventDefault();
                commitRename();
              } else if (e.key === "Escape") {
                e.preventDefault();
                clearPendingBlurCommit();
                isComposingRef.current = false;
                commitAfterCompositionRef.current = false;
                setEditing(false);
              }
            }}
            className="w-full rounded-sm bg-background px-1 py-0.5 text-body font-semibold outline-none ring-1 ring-border focus-visible:ring-brand"
          />
        ) : (
          <button
            type="button"
            onClick={startRename}
            title={t(($) => $.header.rename)}
            className="block max-w-full truncate text-left text-body font-semibold text-foreground outline-none hover:text-foreground/80 focus-visible:text-foreground/80"
          >
            {title}
          </button>
        )}
        {/* hasChannelOrigin also covers isFeishu (a channel_source implies it),
            and keeps the row — and with it the rebind entry — for a Chat whose
            binding row is gone. */}
        {(agent || hasChannelOrigin) && (
          <div className="flex min-w-0 items-center gap-1.5 text-caption text-muted-foreground">
            {agent && (
              <span className="min-w-0 truncate">
                {agent.name}
                {agent.description ? ` · ${agent.description}` : ""}
              </span>
            )}
            <ChatChannelBadge
              session={session}
              variant="header"
              className="max-w-[45%]"
            />
            {canChangeBinding &&
              isFeishu &&
              isCurrentChannelRoute &&
              session.channel_source?.chat_type === "p2p" &&
              session.channel_source?.installation_id &&
              session.channel_source.channel_chat_id && (
                <ChatChannelBindingDialog session={session} sessions={sessions} />
              )}
            {canRebindChannel && <ChatChannelRebindDialog session={session} />}
          </div>
        )}
      </div>

      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <Button
              variant="ghost"
              size="icon-sm"
              className="text-muted-foreground"
              aria-label={t(($) => $.list.row_actions_aria)}
            />
          }
        >
          <MoreHorizontal className="h-4 w-4" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-auto">
          <DropdownMenuItem onClick={startRename}>
            <Pencil className="h-4 w-4" />
            {t(($) => $.header.rename)}
          </DropdownMenuItem>
          {agent && (
            <DropdownMenuItem
              render={<AppLink href={wsPaths.agentDetail(agent.id)} />}
            >
              <UserRound className="h-4 w-4" />
              {t(($) => $.header.view_profile)}
            </DropdownMenuItem>
          )}
          <DropdownMenuSeparator />
          {isArchived ? (
            <>
              <DropdownMenuItem onClick={doUnarchive}>
                <ArchiveRestore className="h-4 w-4" />
                {t(($) => $.header.unarchive)}
              </DropdownMenuItem>
              {/* Hard delete is offered only once a chat is archived. */}
              <DropdownMenuItem variant="destructive" onClick={() => setConfirmDelete(true)}>
                <Trash2 className="h-4 w-4" />
                {t(($) => $.header.delete)}
              </DropdownMenuItem>
            </>
          ) : (
            <DropdownMenuItem onClick={doArchive}>
              <Archive className="h-4 w-4" />
              {t(($) => $.header.archive)}
            </DropdownMenuItem>
          )}
        </DropdownMenuContent>
      </DropdownMenu>

      <AlertDialog open={confirmDelete} onOpenChange={setConfirmDelete}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.session_history.delete_dialog.title)}</AlertDialogTitle>
            <AlertDialogDescription>{title}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.session_history.delete_dialog.cancel)}</AlertDialogCancel>
            <AlertDialogAction
              onClick={doDelete}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {t(($) => $.session_history.delete_dialog.confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <AlertDialog open={confirmArchive} onOpenChange={setConfirmArchive}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t(($) => $.channel.archive_binding.title)}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(($) => $.channel.archive_binding.description)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t(($) => $.channel.archive_binding.cancel)}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                setConfirmArchive(false);
                archiveNow();
              }}
            >
              {t(($) => $.channel.archive_binding.confirm)}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
