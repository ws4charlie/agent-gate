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
sudo -u agentgate-runner /opt/agentgate-runner/venv/bin/python -c 'import gpt_researcher'
sudo -u agentgate-runner test -x /opt/agentgate-runner/bin/runner-gpt-researcher && echo ok
```

Both must work. The runner must not be able to read the gateway's environment file or the console key (`console.key`); check step 5.

## Layout

| Path | Owner | Holds |
|---|---|---|
| `/opt/agentgate-runner/` | root, group `agentgate-runner` | runner program, scripts, venv, Hugging Face cache; no keys |
| `/opt/agentgate-runner/home` | `agentgate-runner`, 0700 | the runner's writable home (`HOME`, `HF_HOME`, `XDG_CACHE_HOME`) |
| `/opt/agentgate` | `agentgate`, group `agentgate-runner`, 0710 | the console's data dir, Claude config, keys |

The runner program sets `HOME`, `HF_HOME` and `XDG_CACHE_HOME` itself, because sudo gives it no home. Set `/opt/agentgate` to 0710 only after its 0755 subfolders are 0700, so the group can pass through but list or read nothing else. The console sets its own `console/` and `runs/` to 0710 and each run directory to 2770.

## 2. Add the sudo rule

```bash
sudo visudo -f /etc/sudoers.d/agentgate-runner
```

An allowlist of variables and one line with the exact program path:

```
Defaults>agentgate-runner env_keep += "AG_RUN_ID AG_RUN_DIR AG_TOPIC AG_INSTRUCTIONS AG_GATEWAY_URL AG_SPIFFE_SELF_FETCH AG_SPIFFE_SOCKET AG_SPIFFE_EXPECT_ID AG_SPIRE_AGENT_BIN LANG LC_ALL TERM TMPDIR"
agentgate ALL=(agentgate-runner) NOPASSWD: /opt/agentgate-runner/bin/runner-gpt-researcher
```

This list is `runnerSudoEnv` in `console/runner.go`; the console passes nothing else. It never passes `HOME`, `PATH` or `USER`: sudo sets those for the target user and refuses them from the caller. No `SETENV`, no capability, no setuid binary.

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
| `--runner-user-runners` | `AG_RUNNER_USER_RUNNERS` | required with `--runner-user`: the runners (names from `--runners`) that start as that user, e.g. `gpt-researcher`. Others, like `claude-code`, keep starting as `agentgate`. |
| `--runner-group` | `AG_RUNNER_GROUP` | optional; default is the runner user's own group |
| `--spire-agent-bin` | `AG_SPIRE_AGENT_BIN` | optional; default `/opt/spire/bin/spire-agent` |

The existing `--runner-spiffe-id`, `--runner-gateway-url` and `--spiffe-socket` stay. With `--runner-user` the console does not fetch a certificate; it tells the runner where the socket is and which ID to expect. On start the console gives its data directory and `runs/` to the group with mode 0710 (the runner can pass through, not list; `console.key` keeps mode 0600) and deletes any leftover `.svid-*` directories from the old mode.

## 5. Verify

```bash
# the runner is not the gateway: only the runner user gets the runner certificate
sudo -u agentgate-runner /opt/spire/bin/spire-agent api fetch x509 -socketPath /run/spire/agent/sockets/agent.sock | grep 'SPIFFE ID'
# the runner cannot read the console key or the gateway's environment
sudo -u agentgate-runner cat /opt/agentgate/console/console.key        # must say: Permission denied
```

Then start a research task from the console. It should finish with a report; the run's log shows `certificate fetched (spiffe://agentgate.internal/runner)`, and the gateway's audit shows the agent as the one mapped to that ID. Start a second task and stop it: the run shows as killed within a few seconds, and no `agentgate-runner` process is left (`pgrep -u agentgate-runner`).

## Killing a run

The console cannot signal another user's processes. Stopping a run sends SIGTERM to `sudo`, which relays it to the runner; the runner's wrapper and Python stop at once and delete the certificate. If the run is still alive after 10 seconds the console sends SIGKILL to `sudo`, which cannot reach the runner.

A runner that ignores SIGTERM therefore keeps running until its own timeout. It is still cut off: the console marks the run killed in the gateway first (`runs.Store.Kill`), and the gateway refuses every later request that names a killed run, so the runner gets no model call, tool call or payment.

## Roll back

1. Start the console without `--runner-user` (and unset `AG_RUNNER_USER`). Runs go back to the old behaviour: the console fetches the certificate as `agentgate`.
2. Move the SPIRE entry back to `unix:uid:$(id -u agentgate)`.
3. Remove `/etc/sudoers.d/agentgate-runner`. Run directories made in the meantime are readable by `agentgate` through the group; no cleanup is needed.
