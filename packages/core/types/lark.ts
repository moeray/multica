/** A Lark Bot installation bound to a single Multica agent.
 *
 * Wire shape mirrors `LarkInstallationResponse` in
 * `server/internal/handler/lark.go`. New fields the backend adds in the
 * future MUST default to optional so older desktop builds keep parsing
 * the response — see CLAUDE.md → API Response Compatibility. */
export interface LarkInstallation {
  id: string;
  workspace_id: string;
  agent_id: string;
  app_id: string;
  tenant_key?: string | null;
  bot_open_id: string;
  installer_user_id: string;
  status: "active" | "revoked" | string;
  /** Which Lark cloud the bot lives on: "feishu" (mainland) or "lark"
   * (international). Auto-detected at install time. Optional so an older
   * desktop build parsing a newer server — or a newer build hitting a
   * server that predates the field — defaults to Feishu in the UI
   * (see CLAUDE.md → API Response Compatibility). */
  region?: "feishu" | "lark" | string;
  /** "unavailable" when Lark refuses to disclose the bot's union_id
   * (the app lacks the contact permission). Not an error state: inbound
   * @-mentions fall back to open_id matching, which only misroutes when
   * several bots share one group chat. Absent once the union_id resolved. */
  bot_union_id_status?: "unavailable" | string;
  /** Lark's diagnostic for `bot_union_id_status`, e.g.
   * `code=41050 msg="no user authority error"`. */
  bot_union_id_reason?: string;
  installed_at: string;
  created_at: string;
  updated_at: string;
}

export interface ListLarkInstallationsResponse {
  installations: LarkInstallation[];
  /** Whether the deployment has the at-rest secret key configured. When
   * false the Bind button must be disabled and the panel renders an
   * empty / "ask the operator to enable Lark" state. */
  configured: boolean;
  /** Whether new installs via the device-flow scan-to-bind path can
   * complete end-to-end — i.e. the device-flow RegistrationService is
   * wired AND the real Lark HTTP APIClient (not the no-op stub) is in
   * place. When false the install entry points are hidden and the
   * panel surfaces a "coming soon" notice. Optional so older desktop
   * builds receiving a server that does not yet emit the field
   * default to `undefined`, treated as not supported. */
  install_supported?: boolean;
}

/** First half of the device-flow install: the server has opened a
 * registration session against accounts.feishu.cn and returned the QR
 * URL. The frontend renders `qr_code_url` as a QR (and as a clickable
 * link fallback) and starts polling `/install/{session_id}/status` at
 * the supplied cadence until success or terminal failure. */
export interface BeginLarkInstallResponse {
  session_id: string;
  qr_code_url: string;
  expires_in_seconds: number;
  poll_interval_seconds: number;
}

/** Status polling result. `status` is the discriminator. */
export interface LarkInstallStatusResponse {
  status: "pending" | "success" | "error" | string;
  /** Populated when status === "success". The frontend invalidates the
   * installations cache so the new row appears in the Settings tab. */
  installation_id?: string;
  /** Stable code on error — switch on this (NOT error_message) to pick
   * the right copy. Common values: "expired", "access_denied",
   * "lark_protocol_error", "bot_info_failed", "installation_conflict",
   * "installer_bind_failed", "internal_error". */
  error_reason?: string;
  /** Human-readable error tail for debugging; the production UI should
   * surface the copy keyed off error_reason and use this only as a
   * diagnostic tooltip. */
  error_message?: string;
}

export interface RedeemLarkBindingTokenResponse {
  workspace_id: string;
  installation_id: string;
  lark_open_id: string;
}

/** Explicit inbox notification destination for one workspace member. */
export interface InboxLarkDestination {
  id: string;
  installation_id: string;
  channel_user_id: string;
  /** Effective state: active, disabled, or unavailable (revoked bot). */
  status: "active" | "disabled" | "unavailable" | string;
  kind: "agent" | "inbox" | string;
  agent_id?: string;
  bot_open_id: string;
  region?: "feishu" | "lark" | string;
  last_test_status?: "success" | "failed" | string;
  last_test_error?: string;
  last_test_at?: string;
  created_at: string;
  updated_at: string;
}

/** A bot the current member may explicitly choose as their inbox outlet. */
export interface InboxLarkTarget {
  installation_id: string;
  kind: "agent" | "inbox" | string;
  agent_id?: string;
  channel_user_id: string;
  bot_open_id: string;
  region?: "feishu" | "lark" | string;
}

export interface InboxLarkDelivery {
  id: string;
  inbox_item_id: string;
  installation_id: string;
  channel_user_id: string;
  status: "pending" | "sending" | "retry" | "delivered" | "failed" | "cancelled" | string;
  attempts: number;
  next_attempt_at?: string;
  last_error?: string;
  cancelled_reason?: string;
  created_at: string;
  updated_at: string;
  delivered_at?: string;
}

export interface LarkInboxSettingsResponse {
  configured: boolean;
  install_supported?: boolean;
  destination: InboxLarkDestination | null;
  targets: InboxLarkTarget[];
  deliveries: InboxLarkDelivery[];
}

export interface LarkInboxTestResponse {
  ok: boolean;
  error?: string;
}
