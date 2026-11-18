# Offline tests for the runner's TLS settings: python3 -m unittest discover scripts/runners
import importlib.util, os, shutil, ssl, subprocess, tempfile, unittest

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


if __name__ == "__main__":
    unittest.main()
