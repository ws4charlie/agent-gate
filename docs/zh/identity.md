# 身份：证书还是 API 密钥

[English](../identity.md)

每个 Agent 都要证明自己是谁。能用证书就用证书，不能用的再用 API 密钥。某个 Agent 的证书跑通后，就把它的密钥关掉，这样它只有一条入口，而不是两条。

## 证书（SPIFFE）

SPIFFE 证书有效期约一小时，并会自动续期，所以泄露的证书很快就失效。网关有一个可选的双向 TLS 端口，只接受你列出的信任域签发的证书。你把每个证书 ID 映射到一个 Agent；该端口上的请求就是这个 Agent，请求里带的密钥会被忽略。

SPIRE 能认证该工作负载时用它：

- **我们自己的研究任务。** 每次研究任务是一个 systemd 单元，用自己的 unix 用户运行，并自己获取证书。见 [runner-user](../../deploy/runner-user.md)。
- **客户自己的 SPIRE。** 让两边的 SPIRE 服务器联邦，在网关上列出两个信任域，再把客户的 ID 映射到 Agent。见 [SPIRE](../../deploy/spire/README.md)。

## API 密钥

拿不到证书的客户端用密钥，比如 Claude Code，或没有 SPIRE 的客户。可以这样让它更安全：

| 措施 | 做法 |
|---|---|
| 过期 | 用 `--key-ttl` 或 `agent key-expiry` 设置，过期的密钥会被拒绝。默认永不过期。 |
| 轮换 | `agentgate agent rotate-key` 只显示一次新密钥；旧密钥再用 24 小时（`--grace`）。 |
| 提醒 | 密钥最后 7 天内，或正在用旧密钥时，每个响应都带 `X-AgentGate-Key-Expires` 头。 |
| 限额 | 每个 Agent 有自己的预算、范围和审批规则。见[策略](policy.md)。 |

## 关掉密钥

证书跑通后，执行 `agentgate agent api-key <agent> off`。该 Agent 的所有密钥随即被拒绝。

SPIRE 宕机时，网关仍在普通端口上服务，用密钥的 Agent 不受影响；只用证书的 Agent 要等 SPIRE 恢复才能连上。绝不能被锁在门外的 Agent，请保留它的密钥。

## 去哪里看

[网关](gateway.md) · [策略](policy.md) · [SPIRE 部署](../../deploy/spire/README.md) · [Runner 用户](../../deploy/runner-user.md)
