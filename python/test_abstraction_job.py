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

    def test_opaque_bytes_survive(self):
        """[JOB-E7]. A spec whose every scalar is spelled a way this module
        would not choose: a redundant solidus in a value and in a key, a
        trailing zero, an exponent, a negative zero, an integer past float64,
        and members in an order nothing would sort them into. Storing 1.5 for
        the 1.50 it was handed rewrites somebody else's document for them."""
        hostile = (
            r'{"esc":"a\/b","o\/d":1,"ratio":1.50,"exp":1e2,'
            r'"zed":-0.0,"big":12345678901234567890,"zebra":1,"apple":2}'
        )
        r = self.sample()
        r.spec = abstraction_job.parse_opaque(hostile)
        got = self.store.load(self.store.submit(r))
        written = got.to_json().decode()
        # [JOB-E1] indents the whole file, so the payload's own whitespace is
        # the record writer's to choose and its tokens are not.
        flat = "".join(line.strip() for line in written.splitlines())
        self.assertIn(hostile, flat.replace('": ', '":').replace(", ", ","))
        self.assertEqual(got.spec["big"], 12345678901234567890)
        self.assertEqual(got.spec["esc"], "a/b")

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


BASE = "abstraction.job/base@1"
INTENT = "abstraction.job/intent@1"
DELEGATION = "abstraction.job/delegation@1"
STEP = "abstraction.job/step@1"
RANGES = "abstraction.download/ranges@1"


def record_text(content=(BASE,), critical=(BASE,), spec='{"artifact":{"bytes":8}}', extra=""):
    names = lambda ns: ",".join('"%s"' % n for n in ns)
    return (
        '{"content":[%s],"critical":[%s],"id":"j1","kind":"download","state":"running",'
        '"spec":%s,%s'
        '"progress":{"done":1,"total":8,"updated_at":"2026-08-20T05:07:14.951609Z"},'
        '"lease":{"owner":"w","epoch":2,"expires_at":"2026-08-20T05:08:14.635068Z"},'
        '"created_at":"2026-08-20T05:07:10.967343Z",'
        '"updated_at":"2026-08-20T05:07:15.134811Z"}'
    ) % (names(content), names(critical), spec, extra)


class CriticalTest(unittest.TestCase):
    """The rules that read `critical`, in the order [JOB-D10] fixes them in.

    Mirrors job/go/refusals_test.go. Two readers that disagree about which
    records are readable are not two implementations of one contract.
    """

    def read(self, text):
        return Record.from_json(text.encode())

    def test_a_critical_ranges_marking_is_stripped_rather_than_refused(self):
        """[JOB-C2]. A reader that knows nothing about `verified` resumes from
        the prefix and re-fetches the rest, so a writer that marked the model
        critical must not be able to stop one. Removed, not refused, and gone
        from what this reader writes back."""
        r = self.read(record_text(content=(BASE,), critical=(BASE, RANGES)))
        self.assertNotIn(RANGES, r.critical)
        self.assertNotIn(RANGES, r.to_json().decode())

    def test_a_critical_step_marking_is_stripped_rather_than_refused(self):
        """The other row the names table marks never. A step is for telling a
        person what is happening; refusing a job over one is a decoration
        stopping work."""
        r = self.read(record_text(content=(BASE, STEP), critical=(BASE, STEP)))
        self.assertNotIn(STEP, r.critical)

    def test_stripping_is_not_permission_to_ignore_criticality(self):
        """[JOB-D1]. A name this reader has never heard of still refuses the
        whole record, and the verdict is unknown-model rather than invalid: the
        record is fine and this reader is too old [JOB-D7]."""
        with self.assertRaises(UnknownSchema):
            self.read(record_text(
                content=(BASE, "example.com/something@1"),
                critical=(BASE, "example.com/something@1"),
            ))

    def test_a_critical_name_outside_content_is_not_a_subset(self):
        """[JOB-D8], and RFC 9052 3.1 behind it: a label listed as critical
        whose parameter is not carried is a fatal error, not a shrug."""
        with self.assertRaises(Invalid):
            self.read(record_text(content=(BASE,), critical=(BASE, INTENT)))

    def test_a_stripped_name_is_not_checked_for_the_subset_rule(self):
        """Stripping first is what makes [JOB-C2] true against [JOB-D8]. Order
        the two the other way and this record refuses."""
        r = self.read(record_text(content=(BASE,), critical=(BASE, RANGES, STEP)))
        self.assertEqual(r.critical, [BASE])

    def test_intent_is_critical_when_present(self):
        """[JOB-I7] with [JOB-I9]: a reader too old to know the intent model
        must refuse rather than keep working on a job somebody asked to stop. So
        marking intent is correct, not an error."""
        r = self.read(record_text(
            content=(BASE, INTENT), critical=(BASE, INTENT),
            extra='"intent":{"want":"cancel"},',
        ))
        self.assertIn(INTENT, r.critical)

    def test_delegation_is_critical_when_present(self):
        r = self.read(record_text(
            content=(BASE, DELEGATION), critical=(BASE, DELEGATION),
            extra='"delegation":{"system":"s","external_id":"e"},',
        ))
        self.assertIn(DELEGATION, r.critical)

    def test_delegation_refuses_a_field_it_does_not_know(self):
        """[JOB-F1]. The scope a definition profile granted and dropped: a newer
        writer's addition here was accepted and destroyed on the next write."""
        with self.assertRaises(Invalid):
            self.read(record_text(
                content=(BASE, DELEGATION), critical=(BASE, DELEGATION),
                extra='"delegation":{"system":"s","external_id":"e","settled_at":1},',
            ))

    def test_a_declaration_list_must_hold_strings(self):
        with self.assertRaises(Invalid):
            self.read(record_text().replace('"content":[', '"content":"', 1)
                      .replace('"abstraction.job/base@1"],', 'abstraction.job/base@1",', 1))


class OpaqueTest(unittest.TestCase):
    """[JOB-E8] and [JOB-E9]: what a payload must satisfy, and what it keeps.

    The two pull against each other. A name is compared after its escapes are
    decoded and the value is written back with them, so a test that only checks
    the refusal cannot see that the comparison rewrote the payload.
    """

    def read(self, text):
        return Record.from_json(text.encode())

    def test_a_repeated_member_name_is_refused(self):
        """At the record's own top level. Go keeps the last, Python kept the
        last, the C++ library that used to sit here kept the first -- one
        record, three documents."""
        with self.assertRaises(Invalid):
            self.read(record_text().replace('"id":"j1"', '"id":"j1","id":"j2"', 1))

    def test_a_repeated_name_inside_an_opaque_value_is_refused(self):
        with self.assertRaises(Invalid):
            self.read(record_text(spec='{"sources":["a"],"sources":["b"]}'))

    def test_two_spellings_of_one_name_are_one_name(self):
        r"""Compared after escape decoding, so "x" and "x" collide."""
        with self.assertRaises(Invalid):
            self.read(record_text(spec=r'{"x":1,"x":2}'))

    def test_a_repeated_name_three_levels_down_is_refused(self):
        with self.assertRaises(Invalid):
            self.read(record_text(spec='{"a":{"b":[{"c":1,"c":2}]}}'))

    def test_the_same_name_in_two_objects_is_not_a_repeat(self):
        """Siblings, parent and child, two array elements, two cases, and two
        sequences that are equal only after Unicode normalization. Every one of
        these is a document, and refusing any of them would be our rule rather
        than [JOB-E9]'s."""
        for spec in (
            '{"o":{"a":1},"p":{"a":2}}',
            '{"a":{"a":1}}',
            '{"xs":[{"a":1},{"a":2}]}',
            '{"a":1,"A":2}',
            r'{"é":1,"é":2}',
        ):
            with self.subTest(spec=spec):
                self.read(record_text(spec=spec))

    def test_an_unpaired_surrogate_escape_is_refused(self):
        r"""[JOB-E8]. "\ud834" is half a character. Outside a payload [JOB-E6]
        would spell a lost byte as an escape; inside one no reader may rewrite
        what it carries, so refusing is the only answer that invents no second
        policy -- and it is the same answer in both places."""
        for text in (
            record_text().replace('"kind":"download"', r'"kind":"\ud834"', 1),
            record_text().replace('"kind":"download"', r'"kind":"\udd1e"', 1),
            record_text(spec=r'{"n":"\ud834"}'),
        ):
            with self.subTest(text=text[:60]):
                with self.assertRaises(Invalid):
                    self.read(text)

    def test_a_surrogate_pair_is_a_character(self):
        self.read(record_text(spec=r'{"n":"𝄞"}'))

    def test_a_payload_escape_json_does_not_have_is_refused(self):
        r"""[JOB-E8]. "a\qb" is refused; "a\\qb", a backslash then a q, is
        not."""
        with self.assertRaises(Invalid):
            self.read(record_text(spec=r'{"k":"a\qb"}'))
        self.read(record_text(spec=r'{"k":"a\\qb"}'))

    def test_json_extensions_to_the_grammar_are_not_values(self):
        """NaN, Infinity and -Infinity are this interpreter's addition to JSON.
        A record carrying one is a record Go and C++ call malformed."""
        for spec in ('{"n":NaN}', '{"n":Infinity}', '{"n":-Infinity}'):
            with self.subTest(spec=spec):
                with self.assertRaises(Invalid):
                    self.read(record_text(spec=spec))

    def test_opaque_bytes_survive_name_comparison(self):
        """[JOB-E7] against [JOB-E9]. Decoding a name to compare it does not
        license re-encoding the payload from this reader's own parse: the keys
        come back with the escapes they arrived with, and so do the numbers."""
        spec = r'{"a":1.50,"b\/c":1e2,"d":-0.0,"e":123456789012345678901234567890}'
        r = self.read(record_text(spec=spec))
        flat = "".join(line.strip() for line in r.to_json().decode().splitlines())
        self.assertIn(spec, flat.replace('": ', '":').replace(", ", ","))


class RefusalClassTest(unittest.TestCase):
    """Every refusal leaves this door as one of this layer's own classes.

    `invalid` is a verdict the contract names and TypeError is not, so a caller
    catching what this layer refuses caught nothing on these five inputs. The
    verdicts are unchanged — each of these was already declined — only the
    class is. The corresponding C++ hole was Value::Error escaping
    Record::decode, which aborted the process on one corpus fixture.
    """

    def refuse(self, text):
        with self.assertRaises(Invalid):
            Record.from_json(text.encode())

    def test_a_timestamp_this_reader_cannot_read_is_invalid(self):
        """`_parse_time` raised a bare ValueError, and download/python imports
        it by name rather than through a record."""
        for stamp in ("2026-08-20t05:07:14.951609z", "2026-08-20T05:07:14.951609+2:00"):
            with self.subTest(stamp=stamp):
                self.refuse(record_text().replace('"2026-08-20T05:07:14.951609Z"',
                                                  '"%s"' % stamp))

    def test_a_field_of_the_wrong_type_is_invalid(self):
        """A null id, a numeric progress and a progress count spelled as a
        string each left this door as an AttributeError or a TypeError."""
        base = record_text()
        for name, broken in (
            ("id", base.replace('"id":"j1"', '"id":null')),
            ("progress", base.replace(
                '"progress":{"done":1,"total":8,'
                '"updated_at":"2026-08-20T05:07:14.951609Z"}', '"progress":1')),
            ("done", base.replace('"done":1', '"done":"1"')),
        ):
            with self.subTest(field=name):
                self.refuse(broken)


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
