export default {
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
      const ghRes = await fetch(
        `https://api.github.com/repos/${env.GITHUB_REPO}/dispatches`,
        {
          method: "POST",
          headers: {
            Authorization: `Bearer ${env.GITHUB_PAT}`,
            Accept: "application/vnd.github.v3+json",
            "User-Agent": "CLIProxyAPI-Sync-Worker",
          },
          body: JSON.stringify({
            event_type: "deploy-upstream",
            client_payload: { version },
          }),
        }
      );

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

async function tgAPI(env, method, body) {
  return fetch(`https://api.telegram.org/bot${env.TELEGRAM_BOT_TOKEN}/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}
