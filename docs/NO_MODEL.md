# No model

With neither `MODEL_API_KEY` nor `MODEL_API_URL` set, the bot runs without a model, and its startup log says "no model: only commands are answered". Commands still work: `/intro`, `/schedule` and its aliases, and scheduled plain text is posted as written; `/intro` then says it runs no model. Anything else addressed to it, a mention, a reply or a DM, gets no reply, no typing indicator and no model request. Nothing is kept in history, not even group context, and MCP servers aren't started.
