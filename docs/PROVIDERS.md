# Other providers

The bot speaks OpenAI's chat completions format, which most providers and local servers offer: OpenAI, Ollama, LM Studio, vLLM, llama.cpp, Groq, Together, Mistral, DeepSeek, and the compatible endpoints of Gemini and Anthropic. Set `MODEL_API_URL` to the API's base URL (the part before `/chat/completions`), `MODEL` to the model's name there, and `MODEL_API_KEY` if it needs one. For example, a local [Ollama](https://ollama.com), which needs no key:

```
MODEL_API_URL=http://localhost:11434/v1
MODEL=qwen3
MODEL_REASONING=none
```

What differs from OpenRouter:

- OpenRouter's extras are sent only to OpenRouter: the request not to use providers that collect data, and the request for the cost. Read your provider's own data policy.
- `MODEL_REASONING` goes as the standard `reasoning_effort`. Some APIs reject it for a model that doesn't reason; set `none` to leave it out.
- Only OpenRouter's model list says what a model supports, so elsewhere the bot assumes the model takes images and can call tools, as the startup log says. If the API rejects a request with images (a 400 or 422), the bot logs a warning, answers that message without them (the model is told it can't see them) and sends no more images until it restarts; the same goes for tools, and chat commands then answer that they need tools. Usually only that first rejected request is wasted; when a message carried both and the tools were the problem, two are, and images stay on.
- OpenRouter's web search runs only on OpenRouter; elsewhere it is left out, with a warning at startup.
- An `https://` API must offer TLS 1.3. A local one can be plain `http://`.
- Under Docker, `localhost` is the container itself: reach a server on the host with `--add-host=host.docker.internal:host-gateway` and `MODEL_API_URL=http://host.docker.internal:11434/v1`, with the server listening on the Docker bridge address as well as 127.0.0.1 (for Ollama, `OLLAMA_HOST=172.17.0.1:11434`, which the host and its containers can both reach), or with its port firewalled. Ollama has no authentication, so `OLLAMA_HOST=0.0.0.0` lets anyone who can reach the port use it (PRIVACY RISK).
