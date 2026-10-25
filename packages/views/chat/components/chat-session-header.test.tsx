import { beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { ChatSession } from "@multica/core/types";
import enChat from "../../locales/en/chat.json";

const updateMutate = vi.hoisted(() => vi.fn());
const switchBindingMutate = vi.hoisted(() => vi.fn());
const attachBindingMutate = vi.hoisted(() => vi.fn());
const archiveMutate = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/paths", () => ({
  useWorkspacePaths: () => ({ agentDetail: (id: string) => `/agents/${id}` }),
}));

vi.mock("@multica/core/chat/mutations", () => ({
  useUpdateChatSession: () => ({ mutate: updateMutate }),
  useDeleteChatSession: () => ({ mutate: vi.fn() }),
  useSetChatSessionArchived: () => ({ mutate: archiveMutate, mutateAsync: vi.fn() }),
  useSwitchChatSessionChannelBinding: () => ({
    mutateAsync: switchBindingMutate,
    isPending: false,
  }),
  useAttachChatSessionChannelBinding: () => ({
    mutateAsync: attachBindingMutate,
    isPending: false,
  }),
}));

vi.mock("@multica/core/chat", () => ({
  useChatStore: (selector: (state: { setActiveSession: () => void }) => unknown) =>
    selector({ setActiveSession: vi.fn() }),
}));

vi.mock("../../common/actor-avatar", () => ({
  ActorAvatar: () => null,
}));

vi.mock("../../navigation", () => ({
  AppLink: ({ children }: { children: React.ReactNode }) => <a>{children}</a>,
}));

import { ChatSessionHeader } from "./chat-session-header";

const TEST_RESOURCES = { en: { chat: enChat } };
const RENAME_LABEL = enChat.header.rename;
const OUTSIDE_LABEL = "Outside control";

const session: ChatSession = {
  id: "session-1",
  workspace_id: "ws-1",
  agent_id: "agent-1",
  creator_id: "user-1",
  title: "Original title",
  status: "active",
  has_unread: false,
  unread_count: 0,
  last_message: null,
  pinned: false,
  created_at: new Date(0).toISOString(),
  updated_at: new Date(0).toISOString(),
};

function startRename(): HTMLInputElement {
  render(
    <>
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={null} />
      </I18nProvider>
      <button type="button">{OUTSIDE_LABEL}</button>
    </>,
  );
  fireEvent.click(screen.getByTitle(RENAME_LABEL));
  return screen.getByRole("textbox", { name: RENAME_LABEL });
}

function renderHeader(
  headerSession: ChatSession,
  props: {
    sessions?: ChatSession[];
    canChangeBinding?: boolean;
  } = {},
) {
  render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <ChatSessionHeader session={headerSession} agent={null} {...props} />
    </I18nProvider>,
  );
}

describe("ChatSessionHeader rename keyboard behavior", () => {
  beforeEach(() => {
    updateMutate.mockReset();
  });

  it.each([
    ["standard composition signal", { isComposing: true, keyCode: 13 }],
    ["Safari composition signal", { isComposing: false, keyCode: 229 }],
  ])("keeps editing when Enter carries the %s", (_name, eventInit) => {
    const input = startRename();
    fireEvent.change(input, { target: { value: "yanjiu" } });

    fireEvent.keyDown(input, { key: "Enter", ...eventInit });

    expect(updateMutate).not.toHaveBeenCalled();
    expect(input).toBeInTheDocument();
    expect(input).toHaveValue("yanjiu");
  });

  it("submits once on a normal Enter", () => {
    const input = startRename();
    fireEvent.change(input, { target: { value: "Renamed chat" } });

    fireEvent.keyDown(input, { key: "Enter", isComposing: false, keyCode: 13 });

    expect(updateMutate).toHaveBeenCalledTimes(1);
    expect(updateMutate).toHaveBeenCalledWith({
      sessionId: "session-1",
      title: "Renamed chat",
    });
    expect(screen.queryByRole("textbox", { name: RENAME_LABEL })).not.toBeInTheDocument();
  });

  it("defers blur submission until an active composition ends", () => {
    const input = startRename();
    const outside = screen.getByRole("button", { name: OUTSIDE_LABEL });
    fireEvent.change(input, { target: { value: "yanjiu" } });
    fireEvent.compositionStart(input);

    act(() => {
      outside.focus();
    });

    expect(updateMutate).not.toHaveBeenCalled();
    expect(input).toBeInTheDocument();
    expect(outside).toHaveFocus();

    fireEvent.change(input, { target: { value: "研究" } });
    fireEvent.compositionEnd(input);

    expect(updateMutate).toHaveBeenCalledTimes(1);
    expect(updateMutate).toHaveBeenCalledWith({
      sessionId: "session-1",
      title: "研究",
    });
    expect(screen.queryByRole("textbox", { name: RENAME_LABEL })).not.toBeInTheDocument();
  });

  it("closes without saving a partial value when compositionend does not arrive", async () => {
    const input = startRename();
    const outside = screen.getByRole("button", { name: OUTSIDE_LABEL });
    fireEvent.change(input, { target: { value: "yanjiu" } });
    fireEvent.compositionStart(input);

    act(() => {
      outside.focus();
    });

    await waitFor(() => {
      expect(screen.queryByRole("textbox", { name: RENAME_LABEL })).not.toBeInTheDocument();
    });
    expect(updateMutate).not.toHaveBeenCalled();
    expect(screen.getByText("Original title")).toBeInTheDocument();
  });

  it("still submits the current value when focus moves outside", () => {
    const input = startRename();
    const outside = screen.getByRole("button", { name: OUTSIDE_LABEL });
    fireEvent.change(input, { target: { value: "Blurred title" } });

    act(() => {
      outside.focus();
    });

    expect(updateMutate).toHaveBeenCalledTimes(1);
    expect(updateMutate).toHaveBeenCalledWith({
      sessionId: "session-1",
      title: "Blurred title",
    });
    expect(outside).toHaveFocus();
  });

  it.each([
    ["standard composition signal", { isComposing: true, keyCode: 27 }],
    ["Safari composition signal", { isComposing: false, keyCode: 229 }],
  ])("keeps editing when Escape carries the %s", (_name, eventInit) => {
    const input = startRename();
    fireEvent.change(input, { target: { value: "yanjiu" } });

    fireEvent.keyDown(input, { key: "Escape", ...eventInit });

    expect(updateMutate).not.toHaveBeenCalled();
    expect(input).toBeInTheDocument();
    expect(input).toHaveValue("yanjiu");
  });

  it("still cancels the edit on Escape", () => {
    const input = startRename();
    fireEvent.change(input, { target: { value: "Discard me" } });

    fireEvent.keyDown(input, { key: "Escape" });

    expect(updateMutate).not.toHaveBeenCalled();
    expect(screen.queryByRole("textbox", { name: RENAME_LABEL })).not.toBeInTheDocument();
    expect(screen.getByText("Original title")).toBeInTheDocument();
  });
});

describe("ChatSessionHeader Feishu channel state", () => {
  const channelSource = {
    channel_type: "feishu",
    chat_type: "p2p",
    installation_id: "installation-1",
    route_revision: 1,
  };

  it("shows the current binding for an active Feishu route", () => {
    renderHeader({
      ...session,
      channel_source: channelSource,
      is_current_channel_route: true,
    });

    const label = `${enChat.channel.feishu} · ${enChat.channel.current_binding}`;
    expect(screen.getByTitle(label)).toHaveAttribute("aria-label", label);
  });

  it("shows a historical session for a retired Feishu route", () => {
    renderHeader({
      ...session,
      channel_source: channelSource,
      is_current_channel_route: false,
    });

    const label = `${enChat.channel.feishu} · ${enChat.channel.historical_session}`;
    expect(screen.getByTitle(label)).toHaveAttribute("aria-label", label);
  });

  it("does not mark a first-party chat as a Feishu session", () => {
    renderHeader(session);

    expect(
      screen.queryByTitle(
        `${enChat.channel.feishu} · ${enChat.channel.current_binding}`,
      ),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByTitle(
        `${enChat.channel.feishu} · ${enChat.channel.historical_session}`,
      ),
    ).not.toBeInTheDocument();
  });

  it("offers binding changes only on the current route for an authorized caller", () => {
    const current = {
      ...session,
      channel_source: { ...channelSource, channel_chat_id: "chat-1" },
      is_current_channel_route: true,
    };
    renderHeader(current, { canChangeBinding: true });

    expect(
      screen.getByRole("button", { name: enChat.channel.change_binding }),
    ).toBeInTheDocument();
  });

  it("does not offer binding changes for a Feishu group chat", () => {
    renderHeader(
      {
        ...session,
        channel_source: { ...channelSource, chat_type: "group", channel_chat_id: "chat-1" },
        is_current_channel_route: true,
      },
      { canChangeBinding: true },
    );

    expect(
      screen.queryByRole("button", { name: enChat.channel.change_binding }),
    ).not.toBeInTheDocument();
  });

  it("offers rebind instead of a switch for a retired Feishu route", () => {
    renderHeader(
      {
        ...session,
        channel_source: { ...channelSource, channel_chat_id: "chat-1" },
        is_current_channel_route: false,
      },
      { canChangeBinding: true },
    );

    expect(
      screen.getByRole("button", { name: enChat.channel.rebind }),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: enChat.channel.change_binding }),
    ).not.toBeInTheDocument();
  });

  it("keeps the rebind entry for a chat whose binding row was deleted", () => {
    renderHeader(
      { ...session, channel_origin: true },
      { canChangeBinding: true },
    );

    expect(
      screen.getByRole("button", { name: enChat.channel.rebind }),
    ).toBeInTheDocument();
  });

  it("shows neither channel action for a first-party chat", () => {
    renderHeader(session, { canChangeBinding: true });

    expect(
      screen.queryByRole("button", { name: enChat.channel.rebind }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: enChat.channel.change_binding }),
    ).not.toBeInTheDocument();
  });

  it("hides rebind from callers who cannot change bindings", () => {
    renderHeader(
      {
        ...session,
        channel_source: { ...channelSource, channel_chat_id: "chat-1" },
        is_current_channel_route: false,
      },
      { canChangeBinding: false },
    );
    expect(
      screen.queryByRole("button", { name: enChat.channel.rebind }),
    ).not.toBeInTheDocument();
  });

  it("offers rebind on an archived chat that still records its channel conversation", () => {
    renderHeader(
      {
        ...session,
        status: "archived",
        channel_source: { ...channelSource, channel_chat_id: "chat-1" },
        is_current_channel_route: false,
      },
      { canChangeBinding: true },
    );

    expect(
      screen.getByRole("button", { name: enChat.channel.rebind }),
    ).toBeInTheDocument();
    expect(screen.getByTitle(
      `${enChat.channel.feishu} · ${enChat.channel.historical_session}`,
    )).toBeInTheDocument();
  });

  it("keeps the rebind entry on an archived chat whose binding row was deleted", () => {
    renderHeader(
      { ...session, status: "archived", channel_origin: true },
      { canChangeBinding: true },
    );

    expect(
      screen.getByRole("button", { name: enChat.channel.rebind }),
    ).toBeInTheDocument();
  });

  it("shows no rebind on an archived chat with no channel provenance", () => {
    renderHeader({ ...session, status: "archived" }, { canChangeBinding: true });

    expect(
      screen.queryByRole("button", { name: enChat.channel.rebind }),
    ).not.toBeInTheDocument();
  });

  it("keeps historical and read-only Feishu routes free of the switch action", () => {    const { unmount } = render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader
          session={{
            ...session,
            channel_source: { ...channelSource, channel_chat_id: "chat-1" },
            is_current_channel_route: false,
          }}
          agent={null}
          canChangeBinding
        />
      </I18nProvider>,
    );
    expect(
      screen.queryByRole("button", { name: enChat.channel.change_binding }),
    ).not.toBeInTheDocument();
    unmount();

    renderHeader(
      {
        ...session,
        channel_source: { ...channelSource, channel_chat_id: "chat-1" },
        is_current_channel_route: true,
      },
      { canChangeBinding: false },
    );
    expect(
      screen.queryByRole("button", { name: enChat.channel.change_binding }),
    ).not.toBeInTheDocument();
  });
});

describe("ChatSessionHeader archive confirmation", () => {
  beforeEach(() => {
    archiveMutate.mockReset();
  });

  const boundRoute = {
    channel_type: "feishu",
    chat_type: "p2p",
    installation_id: "installation-1",
    channel_chat_id: "chat-1",
    route_revision: 1,
  };

  function openArchiveMenuItem() {
    fireEvent.click(
      screen.getByRole("button", { name: enChat.list.row_actions_aria }),
    );
    fireEvent.click(screen.getByRole("menuitem", { name: enChat.header.archive }));
  }

  it("warns that archiving disconnects Feishu and waits for confirmation", () => {
    const onArchive = vi.fn();
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader
          session={{
            ...session,
            channel_source: boundRoute,
            is_current_channel_route: true,
            channel_origin: true,
          }}
          agent={null}
          onArchive={onArchive}
        />
      </I18nProvider>,
    );

    openArchiveMenuItem();

    expect(onArchive).not.toHaveBeenCalled();
    expect(screen.getByText(enChat.channel.archive_binding.title)).toBeInTheDocument();

    fireEvent.click(
      screen.getByRole("button", { name: enChat.channel.archive_binding.confirm }),
    );
    expect(onArchive).toHaveBeenCalledTimes(1);
  });

  it("archives a chat with no current channel route without asking", () => {
    const onArchive = vi.fn();
    render(
      <I18nProvider locale="en" resources={TEST_RESOURCES}>
        <ChatSessionHeader session={session} agent={null} onArchive={onArchive} />
      </I18nProvider>,
    );

    fireEvent.click(
      screen.getByRole("button", { name: enChat.list.row_actions_aria }),
    );
    fireEvent.click(screen.getByRole("menuitem", { name: enChat.header.archive }));

    expect(onArchive).toHaveBeenCalledTimes(1);
    expect(screen.queryByText(enChat.channel.archive_binding.title)).not.toBeInTheDocument();
  });
});
