"""A recall is the issuer asking for the lease back, and the lease lapsing at
the deadline is what makes it a demand. Mirrors job/go/recall_test.go, run
against every binding this implementation has."""

import tempfile
import unittest
from datetime import datetime, timedelta, timezone

from abstraction_job import (
    COMPLETE,
    FEATURE_RECALL,
    PENDING,
    RUN,
    FileStore,
    Invalid,
    LeaseExpired,
    LeaseHeld,
    MemoryStore,
    Record,
    StaleEpoch,
    Terminal,
)


class Clock:
    def __init__(self):
        self.t = datetime(2026, 9, 6, 12, 0, 0, tzinfo=timezone.utc)

    def __call__(self):
        return self.t

    def add(self, seconds):
        self.t += timedelta(seconds=seconds)


def sample():
    return Record(id="", kind="test-kind", spec={"what": "a thing"})


class RecallTest(unittest.TestCase):
    def bindings(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        for name, make in (("files", lambda c: FileStore(self.dir.name, now=c)),
                           ("memory", lambda c: MemoryStore(now=c))):
            clock = Clock()
            with self.subTest(binding=name):
                yield make(clock), clock

    def test_a_recall_shortens_the_lease_and_the_holder_is_evicted_at_the_deadline(self):
        for store, clock in self.bindings():
            jid = store.submit(sample())
            held = store.claim(jid, "holder", 3600)

            r = store.recall(jid, held.lease.epoch, "yield", "issuer", 10)
            self.assertEqual(r.lease.recall.reason, "yield")
            self.assertEqual(r.wants(), RUN)
            self.assertEqual(r.lease.expires_at, clock.t + timedelta(seconds=10))
            self.assertIn(FEATURE_RECALL, r.critical)

            store.renew(jid, held.lease.epoch, 3600)
            self.assertEqual(store.load(jid).lease.expires_at, clock.t + timedelta(seconds=10))

            def checkpoint(rec):
                rec.progress.done = 8

            store.update(jid, held.lease.epoch, checkpoint)
            with self.assertRaises(LeaseHeld):
                store.claim(jid, "holder", 3600)

            clock.add(11)
            with self.assertRaises(LeaseExpired):
                store.update(jid, held.lease.epoch, lambda rec: None)
            self.assertEqual(len(store.orphans()), 1)
            evicted = store.load(jid)
            self.assertTrue(evicted.lease.recalled())
            self.assertEqual(evicted.lease.owner, "holder")
            self.assertEqual(evicted.progress.done, 8)

            nxt = store.claim(jid, "successor", 3600)
            self.assertFalse(nxt.lease.recalled())
            self.assertNotIn(FEATURE_RECALL, nxt.content)

    def test_a_release_under_recall_is_compliance_and_stays_readable(self):
        for store, _clock in self.bindings():
            jid = store.submit(sample())
            held = store.claim(jid, "holder", 3600)
            store.recall(jid, held.lease.epoch, "yield", "", 60)
            store.release(jid, held.lease.epoch)
            r = store.load(jid)
            self.assertEqual(r.lease.owner, "")
            self.assertTrue(r.lease.recalled())
            self.assertEqual(r.state, PENDING)
            with self.assertRaises(LeaseExpired):
                store.recall(jid, held.lease.epoch, "again", "", 60)

    def test_a_recall_is_refused_where_it_cannot_land(self):
        for store, _clock in self.bindings():
            jid = store.submit(sample())
            with self.assertRaises(LeaseExpired):
                store.recall(jid, 0, "yield", "", 60)
            held = store.claim(jid, "holder", 3600)
            with self.assertRaises(StaleEpoch):
                store.recall(jid, held.lease.epoch + 1, "yield", "", 60)
            with self.assertRaises(Invalid):
                store.recall(jid, held.lease.epoch, "  ", "", 60)

            def finish(rec):
                rec.state = COMPLETE

            store.update(jid, held.lease.epoch, finish)
            with self.assertRaises(Terminal):
                store.recall(jid, held.lease.epoch, "yield", "", 60)

    def test_a_recalled_record_round_trips_byte_for_byte(self):
        for store, _clock in self.bindings():
            jid = store.submit(sample())
            held = store.claim(jid, "holder", 3600)
            store.recall(jid, held.lease.epoch, "doubled: lemonade holds it", "resident-broker", 30)
            raw = store.load(jid).to_json()
            self.assertEqual(Record.from_json(raw).to_json(), raw)
            self.assertIn(b'"recall": {', raw)


if __name__ == "__main__":
    unittest.main()
