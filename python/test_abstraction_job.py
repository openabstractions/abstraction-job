"""Tests for the Python implementation of the job abstraction.

These mirror the Go tests deliberately. Two independent implementations that
pass the same assertions is what makes this an abstraction rather than a file
format with one reader.
"""

import os
import tempfile
import unittest
from datetime import datetime, timedelta, timezone

from abstraction_job import (
    PENDING,
    RUNNING,
    Delegation,
    FileStore,
    Invalid,
    LeaseExpired,
    LeaseHeld,
    Record,
    StaleEpoch,
    UnknownSchema,
)


class Clock:
    """Lease tests that sleep are slow and flaky, and a flaky test on the one
    mechanism preventing two owners writing one file is worse than none."""

    def __init__(self):
        self.t = datetime(2026, 8, 18, 12, 0, 0, tzinfo=timezone.utc)

    def __call__(self):
        return self.t

    def add(self, seconds):
        self.t += timedelta(seconds=seconds)


class JobTest(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.clock = Clock()
        self.store = FileStore(self.dir.name, now=self.clock)

    def sample(self) -> Record:
        # A made-up shape. This module must never need to know what is in a
        # spec, and the fact that an invented one works as well as the real one
        # is the property being tested.
        return Record(
            id="",
            kind="test-kind",
            spec={"what": "a thing", "where": [r"\\nas\share\thing", "D:/thing"]},
        )

    def test_submit_and_load(self):
        jid = self.store.submit(self.sample())
        r = self.store.load(jid)
        self.assertEqual(r.state, PENDING)
        self.assertEqual(r.kind, "test-kind")
        self.assertNotIn("/", jid)
        self.assertNotIn("\\", jid)

    def test_spec_is_opaque(self):
        """A shape this module has never seen, with nesting it has no types for,
        must survive untouched. That is what lets downloading grow mirrors and
        chunk manifests without changing the record every language agrees on."""
        r = self.sample()
        r.spec = {"unheard_of": {"deeply": ["nested", {"values": 42}]}, "n": 7}
        jid = self.store.submit(r)
        got = self.store.load(jid)
        self.assertEqual(got.spec["n"], 7)
        self.assertEqual(got.spec["unheard_of"]["deeply"][1]["values"], 42)

    def test_kind_is_required(self):
        r = self.sample()
        r.kind = ""
        with self.assertRaises(Invalid):
            self.store.submit(r)

    def test_delegation_round_trips(self):
        """A job handed to an external system keeps that system's handle, which
        is the only thing that can find the work again once every process
        involved has exited."""
        jid = self.store.submit(self.sample())
        held = self.store.claim(jid, "python-worker", 60)

        def delegate(r):
            r.delegation = Delegation(system="bits", external_id="{6f8c1a2b}")

        self.store.update(jid, held.lease.epoch, delegate)
        got = self.store.load(jid)
        self.assertTrue(got.delegated())
        self.assertEqual(got.delegation.system, "bits")

        def half(r):
            r.delegation = Delegation(system="bits")

        with self.assertRaises(Invalid):
            self.store.update(jid, held.lease.epoch, half)

    def test_claim_is_exclusive(self):
        jid = self.store.submit(self.sample())
        first = self.store.claim(jid, "python-worker", 60)
        self.assertEqual(first.lease.epoch, 1)
        self.assertEqual(first.state, RUNNING)
        with self.assertRaises(LeaseHeld):
            self.store.claim(jid, "go-worker", 60)

    def test_orphan_is_adopted_after_expiry(self):
        """The SIGKILL case: the first owner never releases anything, it simply
        stops existing, and the job must still become available."""
        jid = self.store.submit(self.sample())
        held = self.store.claim(jid, "python-worker", 30)

        def progress(r):
            r.progress.done = 400
            r.checkpoint = {"proven": 400}

        self.store.update(jid, held.lease.epoch, progress)
        self.assertEqual(self.store.orphans(), [])

        self.clock.add(31)

        orphans = self.store.orphans()
        self.assertEqual(len(orphans), 1)
        adopted = self.store.claim(jid, "go-worker", 60)
        self.assertEqual(adopted.lease.epoch, 2)
        # The successor inherits what the predecessor proved — Temporal's
        # heartbeat-details design, copied.
        self.assertEqual(adopted.checkpoint["proven"], 400)

    def test_zombie_owner_is_refused(self):
        """A process suspended past its lease wakes believing it still owns the
        job. By then someone else has claimed it; the zombie must not write."""
        jid = self.store.submit(self.sample())
        zombie = self.store.claim(jid, "python-worker", 30)
        self.clock.add(31)
        self.store.claim(jid, "go-worker", 60)

        with self.assertRaises(StaleEpoch):
            self.store.update(jid, zombie.lease.epoch, lambda r: None)
        with self.assertRaises(StaleEpoch):
            self.store.renew(jid, zombie.lease.epoch, 60)

    def test_expired_owner_cannot_renew(self):
        jid = self.store.submit(self.sample())
        held = self.store.claim(jid, "python-worker", 30)
        self.clock.add(31)
        with self.assertRaises(LeaseExpired):
            self.store.renew(jid, held.lease.epoch, 60)
        with self.assertRaises(LeaseExpired):
            self.store.update(jid, held.lease.epoch, lambda r: None)
        again = self.store.claim(jid, "python-worker", 60)
        self.assertEqual(again.lease.epoch, 2)

    def test_release_hands_off_immediately(self):
        jid = self.store.submit(self.sample())
        held = self.store.claim(jid, "python-worker", 3600)
        self.store.release(jid, held.lease.epoch)
        nxt = self.store.claim(jid, "go-worker", 60)  # no clock movement at all
        self.assertEqual(nxt.lease.epoch, 2)

    def test_epoch_tokens_do_not_block(self):
        jid = self.store.submit(self.sample())
        for i in range(1, 4):
            r = self.store.claim(jid, "worker", 1)
            self.assertEqual(r.lease.epoch, i)
            self.clock.add(2)
        for i in range(1, 4):
            self.assertTrue(os.path.exists(self.store._epoch_path(jid, i)))

    def test_progress_cannot_be_negative(self):
        jid = self.store.submit(self.sample())
        held = self.store.claim(jid, "python-worker", 60)

        def bad(r):
            r.progress.done = -1

        with self.assertRaises(Invalid):
            self.store.update(jid, held.lease.epoch, bad)

    def test_decode_refuses_unknown_schema(self):
        with self.assertRaises(UnknownSchema):
            Record.from_json(b'{"schema":99,"id":"x","kind":"k","state":"pending","spec":{}}')

    def test_record_is_readable_while_held(self):
        jid = self.store.submit(self.sample())
        held = self.store.claim(jid, "python-worker", 60)

        def progress(r):
            r.progress.done = 250
            r.progress.total = 1000

        self.store.update(jid, held.lease.epoch, progress)

        observer = FileStore(self.dir.name, now=self.clock)
        r = observer.load(jid)
        self.assertEqual(r.progress.done, 250)
        self.assertEqual(r.lease.owner, "python-worker")

    def test_submit_refuses_duplicate_id(self):
        r = self.sample()
        r.id = "fixed-id"
        self.store.submit(r)
        with self.assertRaises(FileExistsError):
            self.store.submit(r)

    def test_round_trip_is_byte_stable(self):
        """Encoding a decoded record must reproduce the bytes exactly. If it does
        not, the two implementations will churn the file against each other and
        no diff of a job's history will mean anything."""
        jid = self.store.submit(self.sample())
        path = self.store._record_path(jid)
        with open(path, "rb") as f:
            original = f.read()
        again = Record.from_json(original).to_json()
        self.assertEqual(original, again)


if __name__ == "__main__":
    unittest.main()
