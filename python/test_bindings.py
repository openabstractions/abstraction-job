"""One body of assertions, run against every binding Python has.

The Go package has had this test for a while and Python could not have it,
because Python had only one thing to run it against. The whole public surface of
this module was ``FileStore`` — a type that names its own binding — so "the
abstraction is real in three languages" was true of the RECORD and not of the
INTERFACE. Two of the three could swap what is underneath; the third could not.

Nothing below knows which binding it is talking to. If any of it had to change
to accommodate one of them, the abstraction was not one.

The memory binding is what answers the fair objection. A service in front of a
FileStore is a transport swap, not a second implementation. ``Memory`` shares no
code with ``FileStore``: the lease, the epoch, the exclusivity, the stale-write
refusal and the rule that a paused job is not an orphan are written again
against a dict, which has neither exclusive create nor atomic rename. If those
semantics were really the filesystem's tricks all along, this is where it shows.
"""

import tempfile
import unittest
from datetime import datetime, timedelta, timezone

from abstraction_job import (
    CANCEL,
    COMPLETE,
    PAUSE,
    RUN,
    FileStore,
    LeaseExpired,
    LeaseHeld,
    Memory,
    NotFound,
    Record,
    Scratch,
    StaleEpoch,
    Store,
    Terminal,
)


class Clock:
    def __init__(self):
        self.t = datetime(2026, 9, 4, 12, 0, 0, tzinfo=timezone.utc)

    def __call__(self):
        return self.t

    def add(self, seconds):
        self.t += timedelta(seconds=seconds)


class BindingsTest(unittest.TestCase):
    def bindings(self):
        """Every binding this implementation offers, each with its own clock.

        The clock is handed back because a lease is a statement about TIME, and
        a test that cannot move time can only check that a lease is granted --
        never that it lapses, which is the half the whole design rests on.
        """
        out = []

        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        files_clock = Clock()
        out.append(("files", FileStore(d.name, now=files_clock), files_clock))

        # Shares no code with the one above.
        memory_clock = Clock()
        out.append(("memory", Memory(now=memory_clock), memory_clock))

        return out

    def test_the_same_assertions_on_every_binding(self):
        for name, store, clock in self.bindings():
            with self.subTest(binding=name):
                self.assertIsInstance(store, Store)
                self.contract(store, clock)

    def contract(self, s, clock):
        # The spec is opaque and must survive untouched, including a shape this
        # module has no type for.
        r = Record(id="", kind="test", spec={"anything": [1, 2, 3], "nested": {"deep": True}})
        job_id = s.submit(r)

        # Readable by anyone, holding no lease. That is what makes work
        # observable from outside.
        got = s.load(job_id)
        self.assertEqual(got.kind, "test")
        self.assertEqual(got.spec["anything"], [1, 2, 3])
        self.assertIs(got.spec["nested"]["deep"], True)

        # Unclaimed work is available.
        self.assertEqual(len(s.orphans()), 1)

        # A claim is exclusive, and the epoch rises.
        claimed = s.claim(job_id, "first", 60)
        self.assertEqual(claimed.lease.epoch, 1)
        with self.assertRaises(LeaseHeld):
            s.claim(job_id, "second", 60)

        # ...and a held job is not available to sweeps.
        self.assertEqual(s.orphans(), [])

        # A lease lapses, and a lapsed one cannot be renewed even by the owner
        # holding the right epoch. That is the sleep case: a process suspended
        # for an hour wakes believing it is still the owner, and letting it renew
        # would land its in-flight work on top of a successor's.
        clock.add(3600)
        with self.assertRaises(LeaseExpired):
            s.renew(job_id, claimed.lease.epoch, 60)
        self.assertEqual(len(s.orphans()), 1, "a lapsed lease must free the job")

        # Take it back for the rest of the contract. Re-claiming is the only way
        # back in, and it bumps the epoch, which is the point.
        clock.add(-3600)
        claimed = s.claim(job_id, "first", 60)
        self.assertEqual(claimed.lease.epoch, 2)

        # A write must present the epoch it holds.
        with self.assertRaises(StaleEpoch):
            s.update(job_id, claimed.lease.epoch - 1, lambda rec: None)

        def progress(rec):
            rec.progress.done = 400
            rec.checkpoint = {"verified_prefix": 400}

        updated = s.update(job_id, claimed.lease.epoch, progress)
        self.assertEqual(updated.progress.done, 400)

        # Intent needs no lease, and that exemption is the feature.
        s.set_intent(job_id, PAUSE, "a-bystander")
        paused = s.load(job_id)
        self.assertEqual(paused.wants(), PAUSE)
        self.assertTrue(paused.paused())
        # Who asked is part of the record: a job sitting against somebody's wish
        # is one of the few things that cannot be worked out from outside.
        self.assertEqual(paused.intent.by, "a-bystander")

        # Releasing hands it back; a paused job still must not be swept up.
        s.release(job_id, claimed.lease.epoch)
        self.assertEqual(s.orphans(), [], "a sweep would resume a paused job")

        # Resumed, it is ordinary work again, and a successor inherits what was
        # proven rather than starting over.
        s.set_intent(job_id, RUN, "a-bystander")
        nxt = s.claim(job_id, "successor", 60)
        self.assertEqual(nxt.lease.epoch, 3)
        self.assertEqual(nxt.checkpoint["verified_prefix"], 400)

        # Terminal is terminal.
        def finish(rec):
            rec.state = COMPLETE

        s.update(job_id, nxt.lease.epoch, finish)
        with self.assertRaises(Terminal):
            s.claim(job_id, "too-late", 60)
        with self.assertRaises(Terminal):
            s.set_intent(job_id, CANCEL, "too-late")

        # Not found is not found.
        with self.assertRaises(NotFound):
            s.load("no-such-job")

    def test_only_the_file_binding_offers_a_local_area(self):
        """A caller must be able to DISCOVER that a binding has no local area
        rather than assume one. This is what the capability being separate buys,
        and it is unfalsifiable while every store is a directory."""
        for name, store, _clock in self.bindings():
            with self.subTest(binding=name):
                if name == "files":
                    self.assertIsInstance(store, Scratch)
                    self.assertTrue(store.root())
                    self.assertIn("x", store.work_path("x"))
                else:
                    self.assertNotIsInstance(
                        store, Scratch,
                        f"the {name} binding must not claim to have a local directory",
                    )


if __name__ == "__main__":
    unittest.main()
