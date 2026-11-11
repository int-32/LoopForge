# Slack bot

A notification bot can be a Slack app instead of an enterprise WeChat bot. It uses the same bot queue, account binding, Main Bot chat and approval buttons; only the transport differs. Slack connects over Socket Mode, so the platform needs no public callback URL.

## Create the Slack app

Create an app at <https://api.slack.com/apps> → *From an app manifest*:

```yaml
display_information:
  name: LoopForge
features:
  bot_user:
    display_name: LoopForge
    always_online: true
  app_home:
    messages_tab_enabled: true
    messages_tab_read_only_enabled: false
oauth_config:
  scopes:
    bot:
      - app_mentions:read
      - chat:write
      - im:history
      - im:read
      - channels:read
      - groups:read
      - users:read
settings:
  event_subscriptions:
    bot_events:
      - app_mention
      - message.im
  interactivity:
    is_enabled: true
  socket_mode_enabled: true
```

Then install it to the workspace, copy the **Bot User OAuth Token** (`xoxb-…`), and under *Basic Information → App-Level Tokens* create a token with the `connections:write` scope (`xapp-…`).

## Configure it in LoopForge

In **Bots**, create a bot with:

- **Bot ID**: the Slack App ID (`A…`).
- **Secret**: both tokens separated by a space, `xoxb-… xapp-…`. A secret containing an `xoxb-` token marks the bot as Slack.
- **Enable chat** to let people talk to Main Bot.

Invite the app to the channels it should post to (`/invite @LoopForge`); the receiving-chat list shows those channels and any direct messages with the app.

## How it behaves

- Direct messages are one conversation per person. In a channel, the app answers only when mentioned, and each thread is its own conversation; a top-level mention starts a thread.
- A "已收到，正在处理…" receipt is edited into progress and then into the final reply.
- Waiting and failed SOP nodes arrive as messages with buttons (approve, retry, send back…). A click runs only when the clicking Slack user has bound a platform account with execute permission on the project; otherwise the buttons stay and the bot replies in the card's thread with the binding link. Give approvers project roles and leave everyone else as viewers.
- The first message from an unbound Slack user returns the account-binding link, which needs `AGENTFLOW_PUBLIC_URL` reachable from that person's browser.

## Model

Bot chat runs on CodeBuddy by default. To run it on another CLI with each sender's own credential, set the bot's model with that CLI's prefix through the API, for example `claude-code:claude-sonnet-5-5`. A Claude subscription token from `claude setup-token` (`sk-ant-oat…`) can be saved as the personal Claude Code key; it is passed to the CLI as `CLAUDE_CODE_OAUTH_TOKEN`.

`AGENTFLOW_SLACK_API_URL` overrides the Slack Web API base URL (tests only).
