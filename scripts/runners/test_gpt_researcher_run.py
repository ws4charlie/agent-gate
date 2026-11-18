# Offline tests for the runner's TLS settings: python3 -m unittest discover scripts/runners
import importlib.util, os, shutil, signal, ssl, subprocess, tempfile, time, unittest
from unittest import mock

here = os.path.dirname(os.path.abspath(__file__))
spec = importlib.util.spec_from_file_location("gpt_researcher_run", os.path.join(here, "gpt_researcher_run.py"))
run = importlib.util.module_from_spec(spec)
spec.loader.exec_module(run)  # defines functions only; main() is not called


@unittest.skipUnless(shutil.which("openssl"), "needs the openssl command to make a test certificate")
class SSLContextTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir, True)
        self.cert, self.key = os.path.join(self.dir, "svid.pem"), os.path.join(self.dir, "svid.key")
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=test",
                        "-keyout", self.key, "-out", self.cert], check=True, capture_output=True)

    def test_context_settings(self):
        ctx = run.build_ssl_context(self.cert, self.key, self.cert)
        self.assertEqual(ctx.verify_mode, ssl.CERT_REQUIRED)
        self.assertFalse(ctx.check_hostname)
        self.assertEqual(len(ctx.get_ca_certs()), 1, "only the bundle is trusted")

    def test_from_env(self):
        self.assertIsNone(run.context_from_env({}))
        ctx = run.context_from_env({"AG_SPIFFE_CERT": self.cert, "AG_SPIFFE_KEY": self.key, "AG_SPIFFE_BUNDLE": self.cert})
        self.assertIsInstance(ctx, ssl.SSLContext)

    def test_missing_bundle_is_an_error(self):
        with self.assertRaises(OSError):
            run.build_ssl_context(self.cert, self.key, os.path.join(self.dir, "none.pem"))


class DropCredentialsTest(unittest.TestCase):
    def test_strips_api_key_headers(self):
        try:
            import httpx
        except ImportError:
            self.skipTest("httpx not installed")
        req = httpx.Request("GET", "https://127.0.0.1/", headers={"Authorization": "Bearer k", "X-Api-Key": "k", "X-AgentGate-Run": "r"})
        run._drop_credentials(req)
        self.assertNotIn("authorization", req.headers)
        self.assertNotIn("x-api-key", req.headers)
        self.assertEqual(req.headers["x-agentgate-run"], "r")


def make_cert(dirname, name, uris):
    """A self-signed certificate whose URI names are `uris`; returns (cert, key) or skips."""
    cert, key = os.path.join(dirname, name + ".pem"), os.path.join(dirname, name + ".key")
    san = ",".join("URI:" + u for u in uris)
    r = subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=t",
                        "-addext", "subjectAltName=" + san, "-keyout", key, "-out", cert], capture_output=True)
    if r.returncode != 0:
        raise unittest.SkipTest("this openssl cannot add a subjectAltName")
    return cert, key


@unittest.skipUnless(shutil.which("openssl"), "needs the openssl command to make test certificates")
class SelfFetchTest(unittest.TestCase):
    """The runner fetches its own certificate. A fake spire-agent copies prepared files; no network."""
    ID = "spiffe://example.org/runner"

    def setUp(self):
        self.dir = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.dir, True)
        self.src = os.path.join(self.dir, "src")
        os.mkdir(self.src)
        self.args = os.path.join(self.dir, "args")
        self.agent = os.path.join(self.dir, "spire-agent")
        with open(self.agent, "w") as f:  # copies src/* to the directory after -write
            f.write('#!/bin/sh\necho "$@" > "%s"\n[ -z "$FAKE_FAIL" ] || { echo "no such entry" >&2; exit 1; }\n'
                    'while [ "$1" != -write ]; do shift; done\ncp "%s"/* "$2"/\n' % (self.args, self.src))
        os.chmod(self.agent, 0o700)
        self.env = {"AG_SPIFFE_SELF_FETCH": "1", "AG_SPIFFE_SOCKET": "unix:///run/spire/agent/sockets/agent.sock",
                    "AG_SPIFFE_EXPECT_ID": self.ID, "AG_SPIRE_AGENT_BIN": self.agent}
        self.work = os.path.join(self.dir, "work")
        os.mkdir(self.work)

    def put(self, n, uris):
        cert, key = make_cert(self.dir, "c%d" % n, uris)
        shutil.copy(cert, os.path.join(self.src, "svid.%d.pem" % n))
        shutil.copy(key, os.path.join(self.src, "svid.%d.key" % n))
        shutil.copy(cert, os.path.join(self.src, "bundle.%d.pem" % n))

    def fetch(self):
        return run.fetch_own_certificate(self.env, self.work)

    def test_picks_the_expected_identity(self):
        self.put(0, ["spiffe://example.org/gateway"])
        self.put(1, [self.ID])
        cert, key, bundle = self.fetch()
        self.assertEqual([os.path.basename(p) for p in (cert, key, bundle)], ["svid.1.pem", "svid.1.key", "bundle.1.pem"])
        with open(self.args) as f:
            self.assertEqual(f.read().split(), ["api", "fetch", "x509", "-socketPath", "/run/spire/agent/sockets/agent.sock", "-write", self.work])

    def test_python_parser_when_openssl_cannot_read_extensions(self):
        self.put(0, [self.ID])
        with mock.patch.object(run.shutil, "which", return_value=None):
            self.assertEqual(run.uri_sans(os.path.join(self.src, "svid.0.pem")), [self.ID])
            self.assertTrue(self.fetch()[0].endswith("svid.0.pem"))

    def test_wrong_identity_is_refused(self):
        self.put(0, ["spiffe://example.org/gateway"])
        with self.assertRaisesRegex(run.SelfFetchError, "no certificate for " + self.ID):
            self.fetch()

    def test_extra_names_are_refused(self):
        self.put(0, [self.ID, "spiffe://example.org/gateway"])
        with self.assertRaises(run.SelfFetchError):
            self.fetch()

    def test_agent_failure_is_reported(self):
        with mock.patch.dict(os.environ, {"FAKE_FAIL": "1"}):
            with self.assertRaisesRegex(run.SelfFetchError, "no such entry"):
                self.fetch()

    def test_missing_agent_program(self):
        self.env["AG_SPIRE_AGENT_BIN"] = os.path.join(self.dir, "nope")
        with self.assertRaisesRegex(run.SelfFetchError, "cannot run"):
            self.fetch()

    def test_directory_is_private_and_removed(self):
        self.put(0, [self.ID])
        with mock.patch.object(run.atexit, "register") as reg:
            cert, key, bundle = run.self_fetch(self.env)
        d = os.path.dirname(cert)
        self.assertEqual(os.stat(d).st_mode & 0o777, 0o700)
        self.assertTrue(os.path.exists(key))
        reg.assert_called_once_with(run._remove_selfdir)
        run._remove_selfdir()
        self.assertFalse(os.path.exists(d))

    def test_directory_is_removed_when_the_fetch_fails(self):
        with mock.patch.dict(os.environ, {"FAKE_FAIL": "1"}), mock.patch.object(run.atexit, "register"):
            with self.assertRaises(run.SelfFetchError):
                run.self_fetch(self.env)
            d = run._selfdir
            run._remove_selfdir()
        self.assertFalse(os.path.exists(d))


@unittest.skipUnless(shutil.which("bash"), "needs bash")
class WrapperTest(unittest.TestCase):
    def test_sigterm_ends_the_wrapper_promptly(self):
        d = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, d, True)
        py = os.path.join(d, "python")
        with open(py, "w") as f:
            f.write("#!/bin/sh\necho started\nexec sleep 30\n")
        os.chmod(py, 0o700)
        env = {"PATH": os.environ["PATH"], "GPTR_PYTHON": py, "AG_RUN_ID": "run_1", "AG_RUN_DIR": d, "AG_TOPIC": "t",
               "AG_GATEWAY_URL": "https://127.0.0.1:1", "AG_SPIFFE_SELF_FETCH": "1"}  # no AGENT_KEY
        p = subprocess.Popen(["bash", os.path.join(here, "gpt-researcher")], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        self.assertEqual(p.stdout.readline().strip(), b"started")
        t0 = time.time()
        p.send_signal(signal.SIGTERM)
        p.wait(timeout=5)
        self.assertLess(time.time() - t0, 3)
        p.stdout.close()
        self.assertNotEqual(p.returncode, 0)


if __name__ == "__main__":
    unittest.main()
