# Telemetry

[中文](zh/telemetry.md)

Every model call and every tool call the gateway handles becomes one OpenTelemetry span, written in the standard GenAI conventions. Any OpenTelemetry backend can read these spans as audit evidence without custom parsing.

## What is recorded

A model call is a span named after the model it asked for. A tool call is a span named after the tool. Each span says which operation it was, which provider or tool server served it, which agent and account made it, how many tokens were used, what it cost, and how it ended. A call that was refused or failed carries an error type and an error status. The upstream's own id for the answer is kept when there is one.

Facts the standard has no name for, such as cost, the policy decision and the run, sit under the `agentgate.` prefix. Other tool-protocol requests, such as listing tools, get a plain span with the same `agentgate.` facts.

## What is never recorded

Prompts, answers, tool arguments and tool results never appear in a span. Spans hold names, ids, counts and status only.

## Turning it on

Spans are off by default and cost almost nothing. Set `AG_OTEL_EXPORTER=stdout` to print them, which is handy while trying things out. Set `AG_OTEL_EXPORTER=otlp` to send them to a collector or a vendor over OTLP/HTTP. The destination and any credentials come from the standard `OTEL_EXPORTER_OTLP_*` environment variables.
