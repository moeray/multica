import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const switchBindingMutate = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/chat/mutations", () => ({
  useSwitchChatSessionChannelBinding: () => ({
    mutateAsync: switchBindingMutate,
    isPending: false,
  }),
}));

import { ChatChannelBindingDialog } from "./chat-channel-binding-dialog";

const TEST_RESOURCES = { en: { chat: enChat } };

function makeSession(overrides: Partial<ChatSession> = {}): ChatSession {
  return {
    id: "session-current",
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
    title: "Current chat",
    status: "active",
    has_unread: false,
    created_at: new Date(0).toISOString(),
    updated_at: new Date(0).toISOString(),
    channel_source: {
      channel_type: "feishu",
      installation_id: "installation-1",
      channel_chat_id: "chat-1",
      route_revision: 1,
    },
    is_current_channel_route: true,
    ...overrides,
  };
}

function renderDialog(sessions: ChatSession[]) {
  render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <ChatChannelBindingDialog session={makeSession()} sessions={sessions} />
    </I18nProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: enChat.channel.change_binding }));
}

describe("ChatChannelBindingDialog", () => {
  beforeEach(() => {
    switchBindingMutate.mockReset();
    switchBindingMutate.mockResolvedValue(makeSession({ id: "session-existing" }));
  });

  it("lists active Chats for the same agent and disables the current and conflicting rows", () => {
    renderDialog([
      makeSession(),
      makeSession({ id: "session-existing", title: "Existing chat", channel_source: undefined, is_current_channel_route: undefined }),
      makeSession({
        id: "session-conflict",
        title: "Other bound chat",
        channel_source: {
          channel_type: "feishu",
          installation_id: "installation-1",
          channel_chat_id: "chat-2",
          route_revision: 1,
        },
        is_current_channel_route: true,
      }),
      makeSession({ id: "session-archived", title: "Archived chat", status: "archived" }),
      makeSession({ id: "session-other-agent", title: "Other agent", agent_id: "agent-2" }),
    ]);

    const dialog = screen.getByRole("dialog");
    const list = within(dialog).getByRole("group", {
      name: enChat.channel.binding_dialog.list_aria,
    });
    expect(within(list).getByRole("button", { name: /Current chat/ })).toBeDisabled();
    expect(within(list).getByRole("button", { name: /Other bound chat/ })).toBeDisabled();
    expect(within(list).getByRole("button", { name: "Existing chat" })).toBeEnabled();
    expect(within(list).queryByText("Archived chat")).not.toBeInTheDocument();
    expect(within(list).queryByText("Other agent")).not.toBeInTheDocument();
  });

  it("submits the selected Chat with the route identity from the current binding", async () => {
    renderDialog([
      makeSession(),
      makeSession({
        id: "session-existing",
        title: "Existing chat",
        channel_source: undefined,
        is_current_channel_route: undefined,
      }),
    ]);

    fireEvent.click(screen.getByRole("button", { name: "Existing chat" }));
    fireEvent.click(screen.getByRole("button", { name: enChat.channel.binding_dialog.confirm }));

    await waitFor(() => {
      expect(switchBindingMutate).toHaveBeenCalledWith({
        sessionId: "session-existing",
        installationId: "installation-1",
        channelChatId: "chat-1",
      });
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
  });

  it("keeps the dialog open and explains a stale or conflicting target", async () => {
    switchBindingMutate.mockRejectedValueOnce(
      Object.assign(new Error("conflict"), { status: 409 }),
    );
    renderDialog([
      makeSession(),
      makeSession({
        id: "session-existing",
        title: "Existing chat",
        channel_source: undefined,
        is_current_channel_route: undefined,
      }),
    ]);

    fireEvent.click(screen.getByRole("button", { name: "Existing chat" }));
    fireEvent.click(screen.getByRole("button", { name: enChat.channel.binding_dialog.confirm }));

    expect(
      await screen.findByRole("alert"),
    ).toHaveTextContent(enChat.channel.binding_dialog.conflict_error);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });
});
