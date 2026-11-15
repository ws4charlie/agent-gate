# SPIRE on one VM

SPIRE gives each workload a short-lived certificate (an SVID) that names it as a SPIFFE ID. The gateway accepts that certificate on its optional mutual-TLS listener and maps the ID to an agent. This folder holds the configuration for a single VM where the server, the agent, the gateway and the runner all live together. Trust domain: `agentgate.internal`.

## 1. Install

Pick a release on <https://github.com/spiffe/spire/releases>, then download the tarball and its `.sha256` file, check it, and unpack it.

```bash
V=<version, e.g. 1.x.y>
cd /tmp
curl -fLO https://github.com/spiffe/spire/releases/download/v$V/spire-$V-linux-amd64-musl.tar.gz
curl -fLO https://github.com/spiffe/spire/releases/download/v$V/spire-$V-linux-amd64-musl.tar.gz.sha256
sha256sum -c spire-$V-linux-amd64-musl.tar.gz.sha256
sudo mkdir -p /opt/spire && sudo tar -xzf spire-$V-linux-amd64-musl.tar.gz -C /opt/spire --strip-components=1
sudo mkdir -p /opt/spire/conf /opt/spire/data/server /opt/spire/data/agent /run/spire/agent/sockets
sudo cp server.conf agent.conf /opt/spire/conf/   # the two files from this folder
```

If `sha256sum` does not print `OK`, stop.

## 2. Start the server

```bash
sudo /opt/spire/bin/spire-server run -config /opt/spire/conf/server.conf &
sudo /opt/spire/bin/spire-server healthcheck
```

## 3. Register the runner

The unix attestor identifies a workload by the uid of its process. Pick the uid the runner runs as (`id -u runner`) and give it an ID. The parent is the agent's own ID, `spiffe://agentgate.internal/node`, which step 4 gives the agent through its join token.

```bash
sudo /opt/spire/bin/spire-server entry create \
  -parentID spiffe://agentgate.internal/node \
  -spiffeID spiffe://agentgate.internal/runner \
  -selector unix:uid:<runner uid>
```

The gateway needs an identity too, so it can present its own certificate to clients: make a second entry with `-spiffeID spiffe://agentgate.internal/gateway` and `-selector unix:uid:<gateway uid>`.

## 4. Start the agent

```bash
sudo /opt/spire/bin/spire-server bundle show | sudo tee /opt/spire/conf/bootstrap.crt >/dev/null
TOKEN=$(sudo /opt/spire/bin/spire-server token generate -spiffeID spiffe://agentgate.internal/node | awk '{print $2}')
sudo /opt/spire/bin/spire-agent run -config /opt/spire/conf/agent.conf -joinToken "$TOKEN" &
```

The token is single use and names the agent `spiffe://agentgate.internal/node`, the parent of the entries in step 3. After the first run the agent keeps its own credentials and does not need the token again.

## 5. Run the gateway with the listener

```bash
agentgate serve --spiffe-listen :18443 --spiffe-trust-domain agentgate.internal
```

The socket path defaults to the one in `agent.conf`; override it with `--spiffe-socket` or `AG_SPIFFE_SOCKET`. The gateway refuses to start if the SPIRE agent does not answer. Keep port 18443 off the public internet.

## 6. Map the ID to an agent

```bash
agentgate agent set-spiffe <agent> spiffe://agentgate.internal/runner
agentgate agent api-key <agent> off     # optional: the certificate becomes the only way in
```

A client then calls `https://<host>:18443` with the certificate from its Workload API. Any API key it sends is ignored.

## Notes

- The files hold no secrets. The server's signing key and the agent's credentials sit under `/opt/spire/data`; keep that directory readable by root only.
- Both services need a systemd unit for a real deployment; that is left to the operator.
- The workload's certificates and the gateway's own certificate renew themselves roughly every half hour.
