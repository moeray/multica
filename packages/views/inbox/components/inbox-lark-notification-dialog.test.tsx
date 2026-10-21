import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi, beforeEach } from "vitest";
import en from "../../locales/en/inbox.json";
import { InboxLarkNotificationDialog } from "./inbox-lark-notification-dialog";

const settingsRef = vi.hoisted(() => ({
  current: {
    configured: true,
    install_supported: true,
    destination: null as unknown,
    targets: [
      {
        installation_id: "installation-1",
        kind: "agent",
        agent_id: "agent-1",
        channel_user_id: "ou_1",
        bot_open_id: "bot_1",
        region: "feishu",
      },
    ],
    deliveries: [] as unknown[],
  },
}));

const mockSetDestination = vi.hoisted(() => vi.fn());
const mockDisableDestination = vi.hoisted(() => vi.fn());
const mockTestDestination = vi.hoisted(() => vi.fn());
const mockRetryDelivery = vi.hoisted(() => vi.fn());
const mockBeginInstall = vi.hoisted(() => vi.fn());
const mockInvalidate = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: settingsRef.current, isLoading: false }),
  useQueryClient: () => ({ invalidateQueries: mockInvalidate }),
  queryOptions: <T,>(opts: T) => opts,
}));

vi.mock("@multica/core/hooks", () => ({
  useWorkspaceId: () => "workspace-1",
}));

vi.mock("@multica/core/workspace/hooks", () => ({
  useActorName: () => ({
    getAgentName: () => "Agent One",
  }),
}));

vi.mock("@multica/core/lark", () => ({
  larkInboxSettingsOptions: () => ({ queryKey: ["lark", "workspace-1", "inbox"] }),
  larkKeys: {
    inboxSettings: () => ["lark", "workspace-1", "inbox"],
    installations: () => ["lark", "workspace-1", "installations"],
  },
}));

vi.mock("@multica/core/api", () => {
  class ApiError extends Error {}
  return {
    ApiError,
    api: {
      setLarkInboxDestination: mockSetDestination,
      disableLarkInboxDestination: mockDisableDestination,
      testLarkInboxDestination: mockTestDestination,
      retryLarkInboxDelivery: mockRetryDelivery,
      beginLarkInboxInstall: mockBeginInstall,
      getLarkInstallStatus: vi.fn(),
    },
  };
});

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

vi.mock("react-qr-code", () => ({
  QRCode: () => <span data-testid="qr-code" />,
}));

vi.mock("../../i18n", () => ({
  useT: () => ({
    t: (accessor: (dict: unknown) => string, params?: Record<string, string>) => {
      const template = accessor(en);
      if (!params) return template;
      return template.replace(
        /\{\{(\w+)\}\}/g,
        (_, key: string) => params[key] ?? "",
      );
    },
  }),
}));

describe("InboxLarkNotificationDialog", () => {
  beforeEach(() => {
    settingsRef.current = {
      configured: true,
      install_supported: true,
      destination: null,
      targets: [
        {
          installation_id: "installation-1",
          kind: "agent",
          agent_id: "agent-1",
          channel_user_id: "ou_1",
          bot_open_id: "bot_1",
          region: "feishu",
        },
      ],
      deliveries: [],
    };
    mockSetDestination.mockReset();
    mockDisableDestination.mockReset();
    mockTestDestination.mockReset();
    mockRetryDelivery.mockReset();
  });

  it("keeps the member on the in-app inbox until they choose a destination", () => {
    render(<InboxLarkNotificationDialog open onOpenChange={vi.fn()} />);

    expect(screen.getByText("No Feishu destination")).toBeInTheDocument();
    expect(screen.getByText(/New inbox items stay in Multica only/)).toBeInTheDocument();
  });

  it("saves the explicitly selected target", async () => {
    const user = userEvent.setup();
    mockSetDestination.mockResolvedValue(settingsRef.current);
    render(<InboxLarkNotificationDialog open onOpenChange={vi.fn()} />);

    await user.click(screen.getByRole("radio", { name: /Agent One/ }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(mockSetDestination).toHaveBeenCalledWith("workspace-1", "installation-1"),
    );
    expect(mockInvalidate).toHaveBeenCalled();
  });

  it("shows test success and failure evidence", async () => {
    const user = userEvent.setup();
    settingsRef.current.destination = {
      id: "destination-1",
      installation_id: "installation-1",
      channel_user_id: "ou_1",
      status: "active",
      kind: "agent",
      agent_id: "agent-1",
      bot_open_id: "bot_1",
      region: "feishu",
      created_at: "2026-10-21T00:00:00Z",
      updated_at: "2026-10-21T00:00:00Z",
    };
    mockTestDestination.mockResolvedValue({ ok: false, error: "lark down" });
    render(<InboxLarkNotificationDialog open onOpenChange={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: "Send test" }));

    await waitFor(() => expect(mockTestDestination).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("Test failed: lark down")).toBeInTheDocument();
  });

  it("offers a retry for a terminal failed delivery", async () => {
    const user = userEvent.setup();
    settingsRef.current.destination = {
      id: "destination-1",
      installation_id: "installation-1",
      channel_user_id: "ou_1",
      status: "active",
      kind: "agent",
      agent_id: "agent-1",
      bot_open_id: "bot_1",
      region: "feishu",
      created_at: "2026-10-21T00:00:00Z",
      updated_at: "2026-10-21T00:00:00Z",
    };
    settingsRef.current.deliveries = [
      {
        id: "delivery-1",
        inbox_item_id: "item-1",
        installation_id: "installation-1",
        channel_user_id: "ou_1",
        status: "failed",
        attempts: 5,
        last_error: "lark down",
        created_at: "2026-10-21T00:00:00Z",
        updated_at: "2026-10-21T00:00:00Z",
      },
    ];
    mockRetryDelivery.mockResolvedValue(undefined);
    render(<InboxLarkNotificationDialog open onOpenChange={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: "Retry" }));

    await waitFor(() =>
      expect(mockRetryDelivery).toHaveBeenCalledWith("workspace-1", "delivery-1"),
    );
  });
});
