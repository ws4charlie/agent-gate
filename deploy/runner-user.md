# Research runs as their own unix user

By default the gateway, the console and the research runs share one unix user (`agentgate`). SPIRE tells workloads apart by uid, so it cannot tell a run from the gateway. With this setup a run is a systemd unit that runs as its own user, `agentgate-runner`, and:

- the runner's certificate (`spiffe://agentgate.internal/runner`) is only issued to processes of that user;
- the console never holds that certificate: the runner fetches it itself;
- a compromised runner cannot read the gateway's or the console's files and keys.

The console keeps its hardening (`NoNewPrivileges`, read-only system). It does not become another user: it asks systemd to start the unit, and one polkit rule lets it start and stop units of that one name pattern.

```
console (agentgate) --systemctl start--> agentgate-run@<run id> (agentgate-runner) --spire-agent--> own certificate --> gateway
                    <--runner.out, report.md in the run directory--
```

Only the runners you list move (for example `gpt-researcher`). Others, like `claude-code`, keep starting as `agentgate`.

## Layout

| Path | Owner | Holds |
|---|---|---|
| `/opt/agentgate-runner/` | root, group `agentgate-runner` | runner program, scripts, venv, Hugging Face cache; no keys |
| `/opt/agentgate-runner/home` | `agentgate-runner`, 0700 | the runner's writable home (`HOME`, `HF_HOME`, `XDG_CACHE_HOME`) |
| `/opt/agentgate` | `agentgate`, group `agentgate-runner`, 0710 | the console's data dir (`/opt/agentgate/console`), Claude config, keys |

Set `/opt/agentgate` to 0710 only after its 0755 subfolders are 0700, so the group can pass through but list or read nothing else. The console sets `console/` and `runs/` to 0710 and each run directory to 0770 with group `agentgate-runner`, and gives the files it writes for the runner that group itself. No setgid: the console's unit forbids it (`RestrictSUIDSGID`). The runner program sets `HOME`, `HF_HOME` and `XDG_CACHE_HOME` itself.

The unit file assumes the console's data directory is `/opt/agentgate/console` and the runner program is `/opt/agentgate-runner/bin/runner-gpt-researcher`. Edit the file if yours differ.

## 1. Create the user and the group

```bash
sudo useradd --system --user-group --no-create-home --shell /usr/sbin/nologin agentgate-runner
sudo usermod -aG agentgate-runner agentgate     # the console's user joins the runner's group
```

`agentgate` must be a member of `agentgate-runner`: restart the console after `usermod`, a running process does not see the new group.

## 2. Install the unit and the polkit rule

```bash
sudo cp deploy/systemd/agentgate-run@.service /etc/systemd/system/
sudo cp deploy/systemd/50-agentgate-runs.rules /etc/polkit-1/rules.d/
sudo systemctl daemon-reload
systemd-analyze verify /etc/systemd/system/agentgate-run@.service
```

polkit reads new rules by itself. The rule lets `agentgate` do `start`, `stop` and `kill` on `agentgate-run@run_<uuid>.service` and nothing else.

The program `runner-gpt-researcher` must end up running `scripts/runners/gpt-researcher` from this repo: it reads `AG_TOPIC_FILE` and `AG_INSTRUCTIONS_FILE`.

## 3. Move the SPIRE entry to the runner's uid

The runner entry selects the uid of `agentgate`. Change it to the runner user and keep the gateway entry on `agentgate`.

```bash
sudo /opt/spire/bin/spire-server entry show -spiffeID spiffe://agentgate.internal/runner     # note the entry ID
sudo /opt/spire/bin/spire-server entry delete -entryID <id>
sudo /opt/spire/bin/spire-server entry create \
  -parentID spiffe://agentgate.internal/node \
  -spiffeID spiffe://agentgate.internal/runner \
  -selector unix:uid:$(id -u agentgate-runner)
```

The socket directory `/run/spire/agent/sockets` must be connectable by the runner.

## 4. Start the console with the new flags

| Flag | Env | Value |
|---|---|---|
| `--runner-user` | `AG_RUNNER_USER` | `agentgate-runner` |
| `--runner-user-runners` | `AG_RUNNER_USER_RUNNERS` | required with `--runner-user`: the runners (names from `--runners`) that run as that user, e.g. `gpt-researcher` |
| `--runner-group` | `AG_RUNNER_GROUP` | optional; default is the runner user's own group |
| `--spire-agent-bin` | `AG_SPIRE_AGENT_BIN` | optional; default `/opt/spire/bin/spire-agent` |

The existing `--runner-spiffe-id`, `--runner-gateway-url` and `--spiffe-socket` stay. For a listed runner the console writes four files into the run directory (`runner.env`, `topic.txt`, `instructions.txt` and an empty `runner.out`, all mode 0660, no secrets), runs `systemctl start --wait agentgate-run@<run id>.service`, and follows `runner.out` for progress. A non-zero exit of the runner is a failed run. On start the console also deletes leftover `.svid-*` directories from the old mode.

## 5. Verify

```bash
# only the runner user gets the runner certificate
sudo -u agentgate-runner /opt/spire/bin/spire-agent api fetch x509 -socketPath /run/spire/agent/sockets/agent.sock | grep 'SPIFFE ID'
# the runner cannot read the console key
sudo -u agentgate-runner cat /opt/agentgate/console/console.key        # must say: Permission denied
# the console's user may not start other units
sudo -u agentgate systemctl start ssh.service                           # must be refused
```

Then start a research task from the console. It should finish with a report; the run's log shows `certificate fetched (spiffe://agentgate.internal/runner)`, and the gateway's audit shows the agent mapped to that ID. Start a second task and stop it: the run shows as killed within a few seconds and `systemctl list-units 'agentgate-run@*'` is empty.

## Killing a run

The console marks the run killed in the gateway first (`runs.Store.Kill`): from then on the gateway refuses every request that names the run, so a stuck runner gets no model call, tool call or payment. Then it runs `systemctl stop agentgate-run@<run id>.service`: systemd sends SIGTERM to the whole unit (the wrapper and Python stop at once and delete the certificate) and SIGKILL after 10 seconds. `RuntimeMaxSec` (21 minutes) ends a unit the console forgot.

## Roll back

1. Start the console without `--runner-user` (and unset `AG_RUNNER_USER`, `AG_RUNNER_USER_RUNNERS`). Runs go back to the old behaviour: the console fetches the certificate as `agentgate` and starts the runner itself. The runner scripts still work that way.
2. Move the SPIRE entry back to `unix:uid:$(id -u agentgate)`.
3. Remove `/etc/systemd/system/agentgate-run@.service` and `/etc/polkit-1/rules.d/50-agentgate-runs.rules`, then `sudo systemctl daemon-reload`.
4. Optional: set `/opt/agentgate` back to 0700. Run directories made in the meantime stay readable by `agentgate` through the group.
