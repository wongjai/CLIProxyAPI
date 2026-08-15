# CLIProxyAPI Upstream Sync — Cloudflare Worker

Two jobs:

1. **Cron (every 6h)** — fires `repository_dispatch{event_type: detect-upstream}`
   at `wongjai/CLIProxyAPI`, which runs the *Upstream Sync — Detect* workflow.
   The workflow deliberately has no `schedule:` trigger: GitHub auto-disables
   scheduled workflows after 60 days of repository inactivity, and this fork
   never gets commits, so detection died silently on 2026-08-09.
2. **Webhook** — Telegram inline keyboard handler. `✅ Approve` fires
   `repository_dispatch{event_type: deploy-upstream, client_payload.version}`,
   which runs *Upstream Sync — Deploy*.

## Deploy

```bash
cd cf-worker
npx wrangler deploy

# Set secrets (one-time)
npx wrangler secret put TELEGRAM_BOT_TOKEN
npx wrangler secret put GITHUB_PAT
npx wrangler secret put TELEGRAM_CHAT_ID   # optional, see below
```

## Set Telegram Webhook

After deploying, point your Telegram bot to this worker:

```bash
curl "https://api.telegram.org/bot<BOT_TOKEN>/setWebhook?url=https://cliproxyapi-sync.<your-cf-subdomain>.workers.dev"
```

## Verify the cron

```bash
npx wrangler deployments list          # confirm the cron trigger is registered
npx wrangler tail                      # watch live; scheduled events show up here
gh run list -R wongjai/CLIProxyAPI --workflow=upstream-detect.yml   # should tick every 6h
```

## Secrets

| Secret | Source |
|---|---|
| `TELEGRAM_BOT_TOKEN` | @BotFather on Telegram |
| `GITHUB_PAT` | GitHub → Settings → Developer settings → Fine-grained PAT → scope: `wongjai/CLIProxyAPI` → permission: Contents (Read and write) |
| `TELEGRAM_CHAT_ID` | Optional. If set, the worker sends a Telegram alert when a cron dispatch fails, so the pipeline can't go stale unnoticed again. |
