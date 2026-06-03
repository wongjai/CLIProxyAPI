# CLIProxyAPI Upstream Sync — Cloudflare Worker

Telegram bot callback handler for the upstream sync workflow.
Receives inline keyboard presses and triggers GitHub Actions deploy.

## Deploy

```bash
cd cf-worker
npx wrangler deploy

# Set secrets (one-time)
npx wrangler secret put TELEGRAM_BOT_TOKEN
npx wrangler secret put GITHUB_PAT
```

## Set Telegram Webhook

After deploying, point your Telegram bot to this worker:

```bash
curl "https://api.telegram.org/bot<BOT_TOKEN>/setWebhook?url=https://cliproxyapi-sync.<your-cf-subdomain>.workers.dev"
```

## Secrets

| Secret | Source |
|---|---|
| `TELEGRAM_BOT_TOKEN` | @BotFather on Telegram |
| `GITHUB_PAT` | GitHub → Settings → Developer settings → Fine-grained PAT → scope: `wongjai/CLIProxyAPI` → permission: Contents (Read and write) |
