/**
 * The server's rules for a channel's destination, mirrored from
 * internal/core/notification/config.go so the form can answer before a round
 * trip. channelModel.test.ts reads that file and fails when they drift apart;
 * the server checks everything again.
 */
export const TEAMS_HOST_SUFFIXES = ["logic.azure.com", "api.powerplatform.com", "webhook.office.com"];
export const DENIED_HEADERS = ["Host", "Content-Length", "Transfer-Encoding", "Connection", "Cookie"];
export const MAX_URL_LENGTH = 2048;
export const MAX_HEADER_COUNT = 20;
export const MAX_HEADER_NAME_LENGTH = 128;
export const MAX_HEADER_VALUE_LENGTH = 4096;
export const MAX_TEMPLATE_LENGTH = 16 * 1024;

/** What a webhook body may refer to: each is a ready-made JSON fragment. */
export const TEMPLATE_VARIABLES = ["msg", "eventJSON", "nodeJSON", "ruleJSON"];

export const DEFAULT_TEMPLATE = `{
  "text": {{.msg}},
  "event": {{.eventJSON}}
}`;
