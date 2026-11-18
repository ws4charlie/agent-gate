package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Runners listed in Config.RunnerUserRunners do not run as the console's user.
// The console asks systemd to start a unit instance, agentgate-run@<run id>, as
// the runner user (deploy/systemd/agentgate-run@.service); polkit lets the
// console start and stop units of that name only. The unit reads the run's
// settings from files in the run directory and writes the runner's output to
// runner.out there, which the console follows.

// unitRunner starts and stops runner units.
type unitRunner interface {
	// Start starts the unit and waits until it ends; the error is its failure.
	Start(ctx context.Context, unit string) error
	// Stop stops the unit; systemd kills everything in it.
	Stop(ctx context.Context, unit string) error
}

// systemctl is the real unitRunner.
type systemctl struct{}

func systemctlArgs(verb string, wait bool, unit string) []string {
	args := []string{"--no-ask-password", "--quiet", verb}
	if wait {
		args = append(args, "--wait") // the exit status is the runner's
	}
	return append(args, unit)
}

func (systemctl) run(ctx context.Context, args []string) error {
	var errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, clip(msg, 300))
		}
		return err
	}
	return nil
}

func (c systemctl) Start(ctx context.Context, unit string) error {
	return c.run(ctx, systemctlArgs("start", true, unit))
}
func (c systemctl) Stop(ctx context.Context, unit string) error {
	return c.run(ctx, systemctlArgs("stop", false, unit))
}

func unitName(runID string) string { return "agentgate-run@" + runID + ".service" }

// viaUnit is true when the named runner runs as the runner user.
func (s *Server) viaUnit(runner string) bool {
	return s.cfg.RunnerUser != "" && slices.Contains(s.cfg.RunnerUserRunners, runner)
}

// selfFetch is on when the runner fetches its own SPIFFE certificate.
func (s *Server) selfFetch() bool {
	return s.cfg.RunnerSPIFFEID != "" && s.cfg.RunnerGatewayURL != ""
}

// unitEnv is every variable a runner unit is given (the unit's EnvironmentFile).
// Topic and instructions go in files: they can hold any characters.
func (s *Server) unitEnv(dir, id string) []string {
	env := []string{"AG_RUN_ID=" + id, "AG_RUN_DIR=" + dir,
		"AG_TOPIC_FILE=" + filepath.Join(dir, "topic.txt"), "AG_INSTRUCTIONS_FILE=" + filepath.Join(dir, "instructions.txt")}
	if s.selfFetch() {
		env = append(env, "AG_GATEWAY_URL="+strings.TrimRight(s.cfg.RunnerGatewayURL, "/"),
			"AG_SPIFFE_SELF_FETCH=1", "AG_SPIFFE_SOCKET="+s.cfg.SPIFFESocket, "AG_SPIFFE_EXPECT_ID="+s.cfg.RunnerSPIFFEID)
		if s.cfg.SpireAgentBin != "" {
			env = append(env, "AG_SPIRE_AGENT_BIN="+s.cfg.SpireAgentBin)
		}
	} else {
		env = append(env, "AG_GATEWAY_URL="+strings.TrimRight(s.cfg.Gateway, "/"))
	}
	return env
}

// envFile renders env for systemd's EnvironmentFile: one KEY="value" per line.
// A value that quoting cannot carry is refused rather than mangled.
func envFile(env []string) ([]byte, error) {
	var b bytes.Buffer
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if strings.ContainsAny(v, "\"\\\n\r") {
			return nil, fmt.Errorf("%s has a character the unit's environment file cannot hold", k)
		}
		fmt.Fprintf(&b, "%s=\"%s\"\n", k, v)
	}
	return b.Bytes(), nil
}

// writeShared writes a file the runner's group can read and write.
func writeShared(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o660); err != nil {
		return err
	}
	return os.Chmod(path, 0o660) // the console's umask must not narrow it
}

// runUnit runs the task as a unit and follows its output file.
func (t *task) runUnit(ctx context.Context, instructions string) {
	id := t.meta.ID
	if !runIDRe.MatchString(id) {
		t.finish(stFailed, "the run id is not one the console made")
		return
	}
	env, err := envFile(t.s.unitEnv(t.dir, id))
	if err == nil {
		for name, data := range map[string][]byte{"topic.txt": []byte(t.meta.Topic), "instructions.txt": []byte(instructions),
			"runner.env": env, "runner.out": nil} { // systemd appends to runner.out, so it must exist and be ours
			if err = writeShared(filepath.Join(t.dir, name), data); err != nil {
				break
			}
		}
	}
	if err != nil {
		t.finish(stFailed, "could not prepare the runner: "+err.Error())
		return
	}
	if t.s.selfFetch() {
		t.emit("step", "the runner fetches its own SPIFFE certificate (%s)", t.s.cfg.RunnerSPIFFEID)
	}
	out, err := os.Open(filepath.Join(t.dir, "runner.out"))
	if err != nil {
		t.finish(stFailed, err.Error())
		return
	}
	defer out.Close()

	unit := unitName(id)
	ended := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { // a kill, the timeout or the console stopping
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = t.s.units.Stop(sctx, unit)
	})
	defer stop()
	var werr error
	go func() {
		werr = t.s.units.Start(context.Background(), unit)
		close(ended)
	}()
	t.read(&tailReader{f: out, done: ended})
	<-ended
	t.conclude(ctx, werr)
}

// tailReader reads a file that is still being written, and ends when done is
// closed and what was written is read.
type tailReader struct {
	f    *os.File
	done <-chan struct{}
}

func (r *tailReader) Read(p []byte) (int, error) {
	finished := false
	for {
		n, err := r.f.Read(p)
		if n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return n, err
		}
		if finished {
			return 0, io.EOF
		}
		select {
		case <-r.done:
			finished = true // one more read for what came in just before the end
		case <-time.After(100 * time.Millisecond):
		}
	}
}
