"""Tests for the Python implementation of the job abstraction.

These mirror the Go tests deliberately. Two independent implementations that
pass the same assertions is what makes this an abstraction rather than a file
format with one reader.
"""

import os
import tempfile
import time
import unittest
from datetime import datetime, timedelta, timezone

import abstraction_job
from abstraction_job import (
    PENDING,
    RUNNING,
    TRANSFERRED,
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
        # The CURRENT epoch's token is what proves this owner holds it.
        self.assertTrue(os.path.exists(self.store._epoch_path(jid, 3)))
        # The spent ones do not survive. This assertion used to be the opposite
        # -- every token ever created had to still be there -- which pinned an
        # unbounded leak: one file per claim, forever. A real store reached 1069
        # files for 17 jobs.
        for i in (1, 2):
            self.assertFalse(
                os.path.exists(self.store._epoch_path(jid, i)),
                "the token for spent epoch %d is still there" % i,
            )

    def test_an_abandoned_claim_token_does_not_brick_the_job(self):
        """A token AHEAD of its record must not make a job unclaimable forever.

        The token is created before the record is written, so a process that dies
        in between leaves a token for an epoch the record never reached. Every
        later claim then computed the same next epoch, found that token, and
        failed -- permanently. Seen on a live store: record at 216, token at 217,
        and a supervisor reporting a healthy sweep every five seconds while the
        job silently failed its claim.
        """
        jid = self.store.submit(self.sample())
        held = self.store.claim(jid, "first", 60)
        self.store.release(jid, held.lease.epoch)

        orphan = self.store._epoch_path(jid, held.lease.epoch + 1)
        with open(orphan, "w") as f:
            f.write("a process that died\n")
        old = time.time() - 2 * abstraction_job._CLAIM_HANDOVER_SECONDS
        os.utime(orphan, (old, old))

        nxt = self.store.claim(jid, "successor", 60)
        self.assertGreater(nxt.lease.epoch, held.lease.epoch)
        self.assertFalse(os.path.exists(orphan), "the abandoned token was never cleaned up")

    def test_a_fresh_claim_token_still_wins(self):
        """Skipping past an epoch somebody is still taking would destroy the
        exclusivity the token exists to provide."""
        jid = self.store.submit(self.sample())
        with open(self.store._epoch_path(jid, 1), "w") as f:
            f.write("mid-flight\n")
        with self.assertRaises(LeaseHeld):
            self.store.claim(jid, "interloper", 60)

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

    def test_transferred_job_is_not_an_orphan(self):
        """A finished job must not look like stranded work.

        TRANSFERRED is deliberately not terminal — the requester still has to
        take delivery — so ``claimable`` says yes once the lease lapses. A
        supervisor sweeping for orphans therefore re-ran a job that was already
        complete and verified. On a NAS that meant re-downloading 313 MB every
        30 seconds, and it would have gone on forever. Found by running it, not
        by reading it.

        Go had the identical bug in the identical place, which is the argument
        for writing the second implementation at all: the contract is what both
        agree on, and neither alone would have shown this was part of it.
        """
        jid = self.store.submit(self.sample())
        rec = self.store.claim(jid, "worker", 30)
        self.store.update(
            jid, rec.lease.epoch, lambda r: setattr(r, "state", TRANSFERRED)
        )
        self.clock.add(3600)  # the lease lapses, as it would after a crash

        self.assertNotIn(
            jid,
            [o.id for o in self.store.orphans()],
            "a transferred job was offered up as an orphan; "
            "a supervisor will download it all over again",
        )
        # But taking delivery of it must still be possible. Only the rescue
        # sweep should leave it alone.
        self.store.claim(jid, "consumer", 30)

    def test_releasing_delegated_job_keeps_it_running(self):
        """Releasing a DELEGATED job must not demote it to pending.

        Delegation deliberately releases the lease straight away — holding it
        would stop anyone else polling or finalising. But release also turned
        RUNNING into PENDING, so a job that BITS or a NAS was actively
        downloading looked, to every supervisor sweeping for stranded work,
        exactly like a job nobody had started. The second tier would fetch the
        same bytes all over again while the first was still going.
        """
        jid = self.store.submit(self.sample())
        rec = self.store.claim(jid, "delegator", 30)

        def delegate(r):
            r.delegation = Delegation(system="nas", external_id="remote-1")
            r.state = RUNNING

        self.store.update(jid, rec.lease.epoch, delegate)
        self.store.release(jid, rec.lease.epoch)

        got = self.store.load(jid)
        self.assertEqual(
            got.state,
            RUNNING,
            "a job running inside another system is not pending",
        )
        self.assertTrue(got.delegated(), "the delegation handle was lost")

        # An undelegated job still goes back to pending, which is what release
        # is for in the ordinary case.
        plain = self.store.submit(self.sample())
        pr = self.store.claim(plain, "worker", 30)
        self.store.release(plain, pr.lease.epoch)
        self.assertEqual(self.store.load(plain).state, PENDING)


if __name__ == "__main__":
    unittest.main()
