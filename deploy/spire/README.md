# SPIRE on one VM

SPIRE gives each workload a short-lived certificate (an SVID) that names it as a SPIFFE ID. The gateway accepts that certificate on its optional mutual-TLS listener and maps the ID to an agent. This folder holds the configuration for a single VM where the server, the agent, the gateway and the runner all live together. Trust domain: `agentgate.internal`.

## 1. Install

Pick a release on <https://github.com/spiffe/spire/releases>, then download the tarball and its checksum file (`_sha256sum.txt`), check it, and unpack it.

```bash
V=<version, e.g. 1.x.y>
cd /tmp
curl -fLO https://github.com/spiffe/spire/releases/download/v$V/spire-$V-linux-amd64-musl.tar.gz
curl -fLO https://github.com/spiffe/spire/releases/download/v$V/spire-$V-linux-amd64-musl_sha256sum.txt
sha256sum -c spire-$V-linux-amd64-musl_sha256sum.txt
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

The socket path defaults to the one in `agent.conf`; override it with `--spiffe-socket` or `AG_SPIFFE_SOCKET`. Keep port 18443 off the public internet.

The gateway never depends on SPIRE to run. If the SPIRE agent does not answer, or has no certificate with the gateway's ID, the gateway logs an error and serves on its plain port as usual. It keeps trying in the background (never more than 30 seconds apart) and opens the mutual-TLS port as soon as a certificate is available. Stopping the gateway closes both ports.

When one unix user owns several IDs (see "Both IDs on one user" below), tell the gateway which one is its own:

```bash
agentgate serve --spiffe-listen 127.0.0.1:18443 --spiffe-trust-domain agentgate.internal \
  --spiffe-id spiffe://agentgate.internal/gateway      # or AG_SPIFFE_ID
```

Without `--spiffe-id` the gateway uses the first certificate the Workload API returns.

## 6. Map the ID to an agent

```bash
agentgate agent set-spiffe <agent> spiffe://agentgate.internal/runner
agentgate agent api-key <agent> off     # optional: the certificate becomes the only way in
```

A client then calls `https://<host>:18443` with the certificate from its Workload API. Any API key it sends is ignored.

## 7. Research runs with a certificate

The console starts each research run as a child process. With these two settings it fetches a fresh certificate for the run from the Workload API first:

```bash
agentgate console ... \
  --runner-spiffe-id spiffe://agentgate.internal/runner \   # or AG_RUNNER_SPIFFE_ID
  --runner-gateway-url https://127.0.0.1:18443               # or AG_RUNNER_GATEWAY_URL
```

The console writes the certificate chain, the private key and the trust bundle into a private directory (mode 0700, files 0600) inside the run's folder and tells the runner where they are. The runner then calls the gateway's mutual-TLS port with that certificate and sends no API key; the gateway sees the `runner` ID and treats the run as the agent mapped to it (step 6). The directory is deleted when the run ends, also when it is killed or fails. The run's event log says "running with a SPIFFE certificate" or, if the certificate could not be fetched, "certificate unavailable, using API key": the run then goes on with the agent's key as before. Each request's trace span carries `agentgate.auth` (`spiffe` or `api_key`), so the two cases can be told apart afterwards.

The console needs the same socket as the gateway (`--spiffe-socket` or `AG_SPIFFE_SOCKET`). With neither setting, runs behave exactly as before.

### Both IDs on one user

On the live VM the gateway, the console, the runner and the paid-tool server all run as one unix user. The unix attestor can only tell workloads apart by user, so both `spiffe://agentgate.internal/gateway` and `spiffe://agentgate.internal/runner` are registered for that uid, and every process of that user is offered both. Each one picks its own by ID: the gateway with `--spiffe-id`, the console for the runner with `--runner-spiffe-id`.

This is acceptable here because it is one trust boundary already: any process of that user can read the gateway's environment and the agent's key. The runner's certificate does not widen that, it only replaces a long-lived key with a short-lived one that is deleted after the run. It does not stop another process of the same user from asking the Workload API for the runner ID. Separate identities need separate users.

## Notes

- The files hold no secrets. The server's signing key and the agent's credentials sit under `/opt/spire/data`; keep that directory readable by root only.
- On the live VM both services run as systemd units that restart on failure. The agent reads the one-time join token from a root-only file on its first start; the file is deleted once the agent is healthy.
- The unix attestor tells workloads apart by user. Two workloads under the same user get the same identity, so give the runner its own user before it gets its own ID.
- If the SPIRE agent is down, the gateway keeps serving its plain port and the mutual-TLS port waits for it. New research runs fall back to the agent's key.
- The workload's certificates and the gateway's own certificate renew themselves roughly every half hour.
