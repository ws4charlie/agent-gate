# Gateway

[中文](zh/gateway.md)

The gateway sits between your agents and the model providers. Agents talk to it the same way they would talk to OpenAI or Anthropic, so most tools work by changing one URL. Claude Code works this way.

![Agents use a gateway key. The gateway holds the real provider key and streams answers back.](images/gateway.png)

The real provider key stays on the gateway. Each agent gets its own key, which is only good for talking to the gateway. If an agent leaks its key or gets hijacked, you shut off that one agent and your provider account is untouched.

When a request comes in, the gateway works out who is calling, checks whether that caller may make the request, and sets aside enough money to cover the worst case. Only then does it call the provider. The answer streams straight back to the agent as it arrives, so the gateway adds very little delay, and it reads the usage figures from the stream on the way past.

Things go wrong all the time with long streaming calls, so most of the care went there. If a stream dies halfway, you pay for what actually arrived. If the provider returns an error, you pay nothing. If the same request is sent twice by accident, the second one is turned away before it costs anything.

The price comes from the best source available: the provider's own figure if it gives one, otherwise our price table, otherwise an estimate that is clearly marked as an estimate.
