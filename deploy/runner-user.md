# Research runs as their own unix user

By default the gateway, the console and the research runs share one unix user (`agentgate`). SPIRE tells workloads apart by uid, so it cannot tell a run from the gateway. With this setup runs get their own user, `agentgate-runner`, and:

- the runner's certificate (`spiffe://agentgate.internal/runner`) is only issued to processes of that user;
- the console never holds that certificate: the runner fetches it itself;
- a compromised runner cannot read the gateway's or the console's files and keys.

The only privilege added is one `sudo` rule that lets `agentgate` start one named program as `agentgate-runner`.

```
console (agentgate) --sudo--> runner-gpt-researcher (agentgate-runner) --spire-agent--> its own certificate --> gateway
```

## 1. Create the user and the shared group

```bash
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin agentgate-runner
sudo usermod -aG agentgate-runner agentgate     # the console's user joins the runner's group
```

The runner's group is `agentgate-runner`. Run directories belong to it (setgid, mode 2770), so both users can read and write there. `agentgate` must be a member: restart the console after `usermod`, a running process does not see the new group.

The runner needs read access to its program and Python:

```bash
sudo -u agentgate-runner /opt/agentgate/venv/bin/python -c 'import gpt_researcher'
sudo -u agentgate-runner test -x /opt/agentgate/bin/runner-gpt-researcher && echo ok
```

Both must work. The runner must not be able to read the gateway's environment file or the console key (`console.key`); check step 5.

## 2. Add the sudo rule

```bash
sudo visudo -f /etc/sudoers.d/agentgate-runner
```

One line, exact path, nothing else:

```
agentgate ALL=(agentgate-runner) NOPASSWD:SETENV: /opt/agentgate/bin/runner-gpt-researcher
```

`SETENV` lets the console pass the run's variables (`--preserve-env`). Give no capability and no setuid binary.

## 3. Move the SPIRE entry to the runner's uid

The runner entry today selects the uid of `agentgate`. Change it to the runner user, and keep the gateway entry on `agentgate`.

```bash
sudo /opt/spire/bin/spire-server entry show -spiffeID spiffe://agentgate.internal/runner     # note the entry ID
sudo /opt/spire/bin/spire-server entry delete -entryID <id>
sudo /opt/spire/bin/spire-server entry create \
  -parentID spiffe://agentgate.internal/node \
  -spiffeID spiffe://agentgate.internal/runner \
  -selector unix:uid:$(id -u agentgate-runner)
```

The socket directory `/run/spire/agent/sockets` must be connectable by the runner (it is world-connectable in `deploy/spire`).

## 4. Start the console with the new flags

| Flag | Env | Value |
|---|---|---|
| `--runner-user` | `AG_RUNNER_USER` | `agentgate-runner` |
| `--runner-group` | `AG_RUNNER_GROUP` | optional; default is the runner user's own group |
| `--spire-agent-bin` | `AG_SPIRE_AGENT_BIN` | optional; default `/opt/spire/bin/spire-agent` |

The existing `--runner-spiffe-id`, `--runner-gateway-url` and `--spiffe-socket` stay. With `--runner-user` the console does not fetch a certificate; it tells the runner where the socket is and which ID to expect. On start the console gives its data directory and `runs/` to the group with mode 0710 (the runner can pass through, not list; `console.key` keeps mode 0600) and deletes any leftover `.svid-*` directories from the old mode.

## 5. Verify

```bash
# the runner is not the gateway: only the runner user gets the runner certificate
sudo -u agentgate-runner /opt/spire/bin/spire-agent api fetch x509 -socketPath /run/spire/agent/sockets/agent.sock | grep 'SPIFFE ID'
# the runner cannot read the console key or the gateway's environment
sudo -u agentgate-runner cat /var/lib/agentgate/console/console.key        # must say: Permission denied
```

Then start a research task from the console. It should finish with a report; the run's log shows `certificate fetched (spiffe://agentgate.internal/runner)`, and the gateway's audit shows the agent as the one mapped to that ID. Start a second task and stop it: the run shows as killed within a few seconds, and no `agentgate-runner` process is left (`pgrep -u agentgate-runner`).

## Killing a run

The console cannot signal another user's processes. Stopping a run sends SIGTERM to `sudo`, which relays it to the runner; the runner's wrapper and Python stop at once and delete the certificate. If the run is still alive after 10 seconds the console sends SIGKILL to `sudo`, which cannot reach the runner.

A runner that ignores SIGTERM therefore keeps running until its own timeout. It is still cut off: the console marks the run killed in the gateway first (`runs.Store.Kill`), and the gateway refuses every later request that names a killed run, so the runner gets no model call, tool call or payment.

## Roll back

1. Start the console without `--runner-user` (and unset `AG_RUNNER_USER`). Runs go back to the old behaviour: the console fetches the certificate as `agentgate`.
2. Move the SPIRE entry back to `unix:uid:$(id -u agentgate)`.
3. Remove `/etc/sudoers.d/agentgate-runner`. Run directories made in the meantime are readable by `agentgate` through the group; no cleanup is needed.
