import { beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const attachBindingMutate = vi.hoisted(() => vi.fn());
const setArchivedMutate = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/chat/mutations", () => ({
  useAttachChatSessionChannelBinding: () => ({
    mutateAsync: attachBindingMutate,
    isPending: false,
  }),
  useSetChatSessionArchived: () => ({
    mutateAsync: setArchivedMutate,
    isPending: false,
  }),
}));

import { ChatChannelRebindDialog } from "./chat-channel-rebind-dialog";

const TEST_RESOURCES = { en: { chat: enChat } };

function makeSession(overrides: Partial<ChatSession> = {}): ChatSession {
  return {
    id: "session-restored",
    workspace_id: "ws-1",
    agent_id: "agent-1",
    creator_id: "user-1",
    title: "Archived chat",
    status: "active",
    has_unread: false,
    created_at: new Date(0).toISOString(),
    updated_at: new Date(0).toISOString(),
    channel_source: {
      channel_type: "feishu",
      chat_type: "p2p",
      installation_id: "installation-1",
      channel_chat_id: "chat-1",
      route_revision: 1,
    },
    is_current_channel_route: false,
    channel_origin: true,
    ...overrides,
  };
}

function renderDialog(session: ChatSession = makeSession()) {
  render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <ChatChannelRebindDialog session={session} />
    </I18nProvider>,
  );
  fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind }));
}

describe("ChatChannelRebindDialog", () => {
  beforeEach(() => {
    attachBindingMutate.mockReset();
    attachBindingMutate.mockResolvedValue(makeSession({ is_current_channel_route: true }));
    setArchivedMutate.mockReset();
    setArchivedMutate.mockResolvedValue(makeSession({ status: "active" }));
  });

  it("reattaches the Chat's own route and closes on success", async () => {
    renderDialog();

    fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind_dialog.confirm }));

    await waitFor(() => {
      expect(attachBindingMutate).toHaveBeenCalledWith({ sessionId: "session-restored" });
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(setArchivedMutate).not.toHaveBeenCalled();
  });

  it("keeps the dialog open and explains a route another Chat now owns", async () => {
    attachBindingMutate.mockRejectedValueOnce(
      Object.assign(new Error("conflict"), { status: 409 }),
    );
    renderDialog();

    fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind_dialog.confirm }));

    expect(
      await screen.findByText(enChat.channel.rebind_dialog.conflict_error),
    ).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("explains the manual recovery path when the binding row is already gone", () => {
    renderDialog(makeSession({ channel_source: undefined, is_current_channel_route: undefined }));

    expect(
      screen.getByText(enChat.channel.rebind_dialog.unavailable_title),
    ).toBeInTheDocument();
    expect(screen.getByText(enChat.channel.rebind_dialog.unavailable)).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: enChat.channel.rebind_dialog.confirm }),
    ).not.toBeInTheDocument();
    expect(attachBindingMutate).not.toHaveBeenCalled();
  });
});

describe("ChatChannelRebindDialog on an archived Chat", () => {
  const archived = () => makeSession({ status: "archived", is_current_channel_route: false });

  beforeEach(() => {
    attachBindingMutate.mockReset();
    attachBindingMutate.mockResolvedValue(
      makeSession({ status: "active", is_current_channel_route: true }),
    );
    setArchivedMutate.mockReset();
    setArchivedMutate.mockResolvedValue(makeSession({ status: "active" }));
  });

  it("warns that rebinding unarchives the Chat first", () => {
    renderDialog(archived());

    expect(
      screen.getByText(enChat.channel.rebind_dialog.archived_notice),
    ).toBeInTheDocument();
    expect(setArchivedMutate).not.toHaveBeenCalled();
    expect(attachBindingMutate).not.toHaveBeenCalled();
  });

  it("unarchives before attaching and closes on success", async () => {
    renderDialog(archived());

    fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind_dialog.confirm }));

    await waitFor(() => {
      expect(attachBindingMutate).toHaveBeenCalledWith({ sessionId: "session-restored" });
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(setArchivedMutate).toHaveBeenCalledWith({
      sessionId: "session-restored",
      archived: false,
    });
    expect(setArchivedMutate.mock.invocationCallOrder[0]!).toBeLessThan(
      attachBindingMutate.mock.invocationCallOrder[0]!,
    );
  });

  it("keeps the Chat unarchived and offers a retry when the attach conflicts", async () => {
    attachBindingMutate
      .mockRejectedValueOnce(Object.assign(new Error("conflict"), { status: 409 }))
      .mockResolvedValueOnce(makeSession({ status: "active", is_current_channel_route: true }));
    renderDialog(archived());

    fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind_dialog.confirm }));

    expect(
      await screen.findByText(enChat.channel.rebind_dialog.conflict_error),
    ).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    // Unarchive committed; the failed attach must not put the Chat back.
    expect(setArchivedMutate).toHaveBeenCalledTimes(1);
    expect(setArchivedMutate).toHaveBeenCalledWith({
      sessionId: "session-restored",
      archived: false,
    });

    fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind_dialog.confirm }));

    await waitFor(() => {
      expect(attachBindingMutate).toHaveBeenCalledTimes(2);
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
  });

  it.each([
    [403, enChat.channel.rebind_dialog.forbidden_error],
    [404, enChat.channel.rebind_dialog.not_found_error],
  ])("surfaces a %i from the attach and keeps the Chat unarchived", async (status, message) => {
    attachBindingMutate
      .mockRejectedValueOnce(Object.assign(new Error(String(status)), { status }))
      .mockResolvedValueOnce(makeSession({ status: "active", is_current_channel_route: true }));
    renderDialog(archived());

    fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind_dialog.confirm }));

    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    // The only route this dialog can ask for is the Chat's own.
    expect(attachBindingMutate).toHaveBeenCalledWith({ sessionId: "session-restored" });
    expect(setArchivedMutate).toHaveBeenCalledTimes(1);
    expect(setArchivedMutate).toHaveBeenCalledWith({
      sessionId: "session-restored",
      archived: false,
    });

    fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind_dialog.confirm }));

    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
  });

  it("does not attach when the unarchive itself fails", async () => {
    setArchivedMutate.mockRejectedValueOnce(
      Object.assign(new Error("forbidden"), { status: 403 }),
    );
    renderDialog(archived());

    fireEvent.click(screen.getByRole("button", { name: enChat.channel.rebind_dialog.confirm }));

    expect(
      await screen.findByText(enChat.channel.rebind_dialog.unarchive_error),
    ).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    expect(attachBindingMutate).not.toHaveBeenCalled();
  });
});
