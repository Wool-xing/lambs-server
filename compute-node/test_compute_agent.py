"""Tests for compute-agent hardening: IP allowlist, token auth, fail-closed."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import importlib.util

_spec = importlib.util.spec_from_file_location(
    "compute_agent", os.path.join(os.path.dirname(os.path.abspath(__file__)), "compute-agent.py"))
compute_agent = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(compute_agent)


class FakeHandler:
    def __init__(self, headers=None):
        self.headers = headers or {}


class TestIPAllowed(unittest.TestCase):
    def test_localhost_allowed(self):
        self.assertTrue(compute_agent.ip_allowed("127.0.0.1"))

    def test_tailscale_allowed(self):
        self.assertFalse(compute_agent.ip_allowed("100.64.0.9"))  # default list empty — env-configured only

    def test_other_cgnat_denied(self):
        # 100.64.0.0/10 was narrowed to app1's Tailscale IP — carrier-grade
        # NAT neighbors in the same /10 must no longer reach the agent.
        self.assertFalse(compute_agent.ip_allowed("100.101.0.5"))

    def test_ipv4_mapped_allowed(self):
        self.assertFalse(compute_agent.ip_allowed("::ffff:100.64.0.9"))

    def test_ipv4_mapped_other_cgnat_denied(self):
        self.assertFalse(compute_agent.ip_allowed("::ffff:100.101.0.5"))

    def test_rfc1918_10x_denied_by_default(self):
        # 10.0.0.0/8 was removed from the default allowlist
        self.assertFalse(compute_agent.ip_allowed("10.0.0.5"))

    def test_public_ip_denied(self):
        self.assertFalse(compute_agent.ip_allowed("8.8.8.8"))

    def test_malformed_ip_denied(self):
        self.assertFalse(compute_agent.ip_allowed("not-an-ip"))
        self.assertFalse(compute_agent.ip_allowed("1.2.3"))
        self.assertFalse(compute_agent.ip_allowed("::ffff:a.b.c.d"))


class TestTokenAuth(unittest.TestCase):
    def setUp(self):
        self.old = compute_agent.COMPUTE_TOKEN
        compute_agent.COMPUTE_TOKEN = "secret123"

    def tearDown(self):
        compute_agent.COMPUTE_TOKEN = self.old

    def test_correct_bearer_accepted(self):
        self.assertTrue(compute_agent.token_ok(FakeHandler({"Authorization": "Bearer secret123"})))

    def test_wrong_token_rejected(self):
        self.assertFalse(compute_agent.token_ok(FakeHandler({"Authorization": "Bearer wrong"})))

    def test_missing_header_rejected(self):
        self.assertFalse(compute_agent.token_ok(FakeHandler()))

    def test_fail_closed_when_token_unset(self):
        compute_agent.COMPUTE_TOKEN = ""
        self.assertFalse(compute_agent.token_ok(FakeHandler({"Authorization": "Bearer anything"})))


if __name__ == "__main__":
    unittest.main()
