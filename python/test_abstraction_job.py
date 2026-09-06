"""Tests for the Python implementation of the job abstraction.

These mirror the Go tests deliberately. Two independent implementations that
pass the same assertions is what makes this an abstraction rather than a file
format with one reader.
"""

import json
import os
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "cas", "python"))

import abstraction_job
from abstraction_job import (
    PENDING,
    RUNNING,
    PAUSE,
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

    def test_a_claim_leaves_only_the_record_and_its_lock(self):
        jid = self.store.submit(self.sample())
        for i in range(1, 4):
            r = self.store.claim(jid, "worker", 1)
            self.assertEqual(r.lease.epoch, i)
            self.clock.add(2)
        self.assertEqual(
            sorted(os.listdir(os.path.join(self.store.root(), "jobs"))),
            [jid + ".json", jid + ".json.lock"],
        )

    def test_a_claim_keeps_what_was_written_since_the_caller_read(self):
        """The record written is the one under the lock, not the caller's copy:
        the old compare-then-rename compared the epoch and wrote the caller's
        copy, which erased an intent set in between while reporting success."""
        jid = self.store.submit(self.sample())
        seen = self.store.load(jid)
        self.store.set_intent(jid, PAUSE, "ui")
        held = self.store.claim_from(seen, "sweeper", 60)
        self.assertEqual(held.wants(), PAUSE)
        r = self.store.load(jid)
        self.assertEqual((r.wants(), r.lease.owner), (PAUSE, "sweeper"))

    def test_a_stale_claim_cannot_commit_over_a_newer_owner(self):
        """A claim computed from a record that has since moved must not commit:
        the epoch would go backwards, a zombie holding the earlier number would
        pass the staleness check in ``update``, and two processes would work one
        job while every step reported success."""
        jid = self.store.submit(self.sample())
        # What a straggler read before it was descheduled: the job, unclaimed.
        seen = self.store.load(jid)

        # Two claims go through while it is not looking.
        zombie = self.store.claim(jid, "first", 30)
        self.clock.add(31)
        current = self.store.claim(jid, "second", 30)
        self.assertEqual(current.lease.epoch, 2, "staging: the record must be at epoch 2")

        # The straggler wakes up and finishes the claim it started.
        with self.assertRaises(LeaseHeld):
            self.store.claim_from(seen, "straggler", 30)

        after = self.store.load(jid)
        self.assertEqual(
            (after.lease.epoch, after.lease.owner),
            (2, "second"),
            "the epoch went backwards and the owner doing the work no longer owns it",
        )

        # The damage that would have followed: the first owner's lease expired
        # and its writes must stay refused. They only stay refused while the
        # epoch on disk is above its own.
        with self.assertRaises(StaleEpoch):
            self.store.update(jid, zombie.lease.epoch, lambda r: setattr(r.progress, "done", 999))

    def test_progress_cannot_be_negative(self):
        jid = self.store.submit(self.sample())
        held = self.store.claim(jid, "python-worker", 60)

        def bad(r):
            r.progress.done = -1

        with self.assertRaises(Invalid):
            self.store.update(jid, held.lease.epoch, bad)

    def test_decode_refuses_an_unknown_field(self):
        """A field nobody here knows is a newer writer far more often than it is
        a typo, and continuing a job whose description we only partly understand
        is the risk this refuses to take.

        Go and C++ have always refused here. This side used to pick out the keys
        it knew and drop the rest, which is worse than either: the next write
        destroyed another participant's data, invisibly, because nobody here
        could read what was lost.
        """
        jid = self.store.submit(self.sample())
        with open(self.store._record_path(jid), "rb") as f:
            good = json.loads(f.read())

        newer = dict(good)
        newer["invented_field"] = {"by": "something newer"}
        with self.assertRaises(Invalid):
            Record.from_json(json.dumps(newer).encode())

        nested = dict(good)
        nested["lease"] = dict(good["lease"], invented_field=1)
        with self.assertRaises(Invalid):
            Record.from_json(json.dumps(nested).encode())

        # And what belongs to somebody else still goes through untouched, which
        # is the escape that makes the refusal affordable.
        carried = dict(good)
        carried["extensions"] = {"nas.transfer/v1": {"anything": [1, 2, 3]}}
        back = Record.from_json(json.dumps(carried).encode())
        self.assertEqual(back.extensions["nas.transfer/v1"], {"anything": [1, 2, 3]})

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
        with self.assertRaises(Invalid):
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


class LayoutTest(unittest.TestCase):
    """Which relative paths belong to the store rather than to whoever writes
    into it. Mirrors job/go/layout_test.go; the two must refuse the same set."""

    def test_reserved_covers_every_path_a_store_writes(self):
        """Read off a real store, not copied from the documentation. A store
        that grows a directory and a `reserved` that does not would tell the
        layer above that a path is free space when it is about to be written."""
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        store = abstraction_job.FileStore(d.name)
        jid = store.submit(Record(id="", kind="download", spec={"what": "a thing"}))
        rec = store.claim(jid, "owner", 60)
        store.update(jid, rec.lease.epoch, lambda r: setattr(r.progress, "done", 1))
        with open(store.work_path(jid), "wb") as fh:
            fh.write(b"a partial")

        stranger = "0000000000000-000000000000"
        found = 0
        for parent, dirs, files in os.walk(d.name):
            for name in list(dirs) + list(files):
                rel = os.path.relpath(os.path.join(parent, name), d.name)
                found += 1
                self.assertTrue(
                    abstraction_job.reserved(stranger, rel),
                    "the store wrote %r and reserved calls it free space" % rel,
                )
        self.assertGreaterEqual(found, 4, "this test is not exercising the store")

        own = os.path.relpath(store.work_path(jid), d.name)
        self.assertFalse(
            abstraction_job.reserved(jid, own),
            "a job was refused its own scratch",
        )

    def test_reserved_names_the_layout_and_nothing_else(self):
        me = "1757000000000-deadbeef"
        other = "1757000000001-cafebabe"
        for p in (
            "jobs",
            "jobs/x.json",
            "jobs/%s.json" % me,
            "jobs/%s.json.lock" % me,
            "jobs/%s.tmp-123" % me,
            "jobs/%s.json.123.tmp" % me,
            "jobs/deeper/still.json",
            "work",
            "work/" + other,
            "work/%s/part" % other,
            "services.json",
            # The spellings a filesystem folds into the ones above.
            "Jobs/x.json",
            "JOBS/x.json",
            "jobs\\x.json",
            "jobs./x.json",
            "WORK/" + other,
            "Services.json",
            "models/../jobs/x.json",
            "./jobs/x.json",
        ):
            self.assertTrue(abstraction_job.reserved(me, p), p)

        for p in (
            "",
            "models/x.gguf",
            "work/" + me,
            "work/%s/part" % me,
            "work/" + me.upper(),
            "jobsy/x.json",
            "myjobs/x.json",
            "a/jobs/x.json",
            "a/services.json",
            "services.json.bak",
            "jobs/../models/x.gguf",
            # download's, not this layer's -- see download.reserved_sink.
            "supervisor.json",
        ):
            self.assertFalse(abstraction_job.reserved(me, p), p)

        # No owner means no job owns anything, so the whole of work/ is the
        # store's. That is what a caller asks before it has been given an id.
        for p in ("work/" + me, "work/" + other):
            self.assertTrue(abstraction_job.reserved("", p), p)


def role(name, path, n):
    store = FileStore(os.path.dirname(os.path.dirname(path)))
    jid = os.path.basename(path)[: -len(".json")]
    epoch = store.load(jid).lease.epoch
    if name == "writer":
        for _ in range(n):
            store.update(jid, epoch, lambda r: setattr(r.progress, "done", r.progress.done + 1))
            store.renew(jid, epoch, 3600)
    elif name == "reader":
        last = 0
        while last < n:
            done = store.load(jid).progress.done
            if done < last:
                sys.exit("progress went backwards: %d after %d" % (done, last))
            last = done


if __name__ == "__main__":
    if "JOB_ROLE" in os.environ:
        role(os.environ["JOB_ROLE"], os.environ["JOB_PATH"], int(os.environ["JOB_N"]))
    else:
        unittest.main()
