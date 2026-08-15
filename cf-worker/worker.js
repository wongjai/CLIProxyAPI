export default {
  // Cron trigger (see wrangler.toml): drives the upstream detect workflow.
  // GitHub auto-disables workflows that carry a `schedule:` trigger after 60
  // days of repository inactivity, and this fork never gets commits (pure
  // upstream tracking). Scheduling from here instead keeps detection alive.
  async scheduled(event, env, ctx) {
    ctx.waitUntil(triggerDetect(env));
  },

  async fetch(request, env) {
    if (request.method !== "POST") {
      return new Response("OK", { status: 200 });
    }

    const update = await request.json();
    if (!update.callback_query) {
      return new Response("OK", { status: 200 });
    }

    const { callback_query } = update;
    const chatId = callback_query.message.chat.id;
    const messageId = callback_query.message.message_id;
    const originalText = callback_query.message.text;
    const [action, version] = callback_query.data.split(":");

    await tgAPI(env, "answerCallbackQuery", {
      callback_query_id: callback_query.id,
    });

    if (action === "approve") {
      const ghRes = await ghDispatch(env, "deploy-upstream", { version });

      const statusLine = ghRes.ok
        ? `\n\n🚀 Deploy triggered for ${version}`
        : `\n\n❌ Failed to trigger deploy (HTTP ${ghRes.status})`;

      await tgAPI(env, "editMessageText", {
        chat_id: chatId,
        message_id: messageId,
        text: originalText + statusLine,
      });
    } else if (action === "skip") {
      await tgAPI(env, "editMessageText", {
        chat_id: chatId,
        message_id: messageId,
        text: originalText + `\n\n⏭ Skipped ${version}`,
      });
    }

    return new Response("OK", { status: 200 });
  },
};

async function triggerDetect(env) {
  const ghRes = await ghDispatch(env, "detect-upstream", {});
  if (ghRes.ok || !env.TELEGRAM_CHAT_ID) {
    return;
  }
  // Silent cron failures are how this pipeline went stale in the first place.
  await tgAPI(env, "sendMessage", {
    chat_id: env.TELEGRAM_CHAT_ID,
    text: `⚠️ CLIProxyAPI sync: could not trigger upstream detect (HTTP ${ghRes.status})`,
  });
}

async function ghDispatch(env, eventType, payload) {
  return fetch(`https://api.github.com/repos/${env.GITHUB_REPO}/dispatches`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${env.GITHUB_PAT}`,
      Accept: "application/vnd.github.v3+json",
      "User-Agent": "CLIProxyAPI-Sync-Worker",
    },
    body: JSON.stringify({
      event_type: eventType,
      client_payload: payload,
    }),
  });
}

async function tgAPI(env, method, body) {
  return fetch(`https://api.telegram.org/bot${env.TELEGRAM_BOT_TOKEN}/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}
