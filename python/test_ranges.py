"""A checkpoint of ranges, in Python.

The record every implementation must produce byte for byte lives in
``testdata/ranges-record.json``. One file, three languages: Go, Python and C++
each build the same record from the same ranges and compare. A conformance test
that compares bytes is the only thing that has ever caught these three
disagreeing.
"""

import json
import os
import tempfile
import unittest
from datetime import datetime, timezone

from abstraction_job import (
    MODEL_RANGES,
    RUNNING,
    FileStore,
    Invalid,
    Range,
    Record,
    canonical_ranges,
    checkpoint_with_ranges,
    ranges_cover,
    ranges_from_checkpoint,
    ranges_missing,
    ranges_total,
    verified_prefix,
)

FIXTURE = os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir,
                       "testdata", "ranges-record.json")


def _at(s: str) -> datetime:
    return datetime.fromisoformat(s.replace("Z", "+00:00"))


# Out of order, and with two adjacent pairs that must merge. A parallel
# fetcher's parts finish in whatever order they finish, and no caller should
# have to sort before recording.
FIXTURE_RANGES = [
    (20971520, 23068672),
    (8388608, 10485760),
    (0, 2097152),
    (10485760, 12582912),
    (2097152, 4194304),
]


def fixture_record() -> Record:
    r = Record(
        id="1787202430967-a752f9a9c2c77b123ffd",
        kind="download",
        state=RUNNING,
        spec={"artifact": {"bytes": 23068672}},
    )
    r.set_checkpoint_ranges(FIXTURE_RANGES)
    r.progress.done = 10485760
    r.progress.total = 23068672
    r.progress.updated_at = _at("2026-08-20T05:07:14.951609Z")
    r.lease.owner = "go-worker"
    r.lease.epoch = 2
    r.lease.expires_at = _at("2026-08-20T05:08:14.635068Z")
    r.created_at = _at("2026-08-20T05:07:10.967343Z")
    r.updated_at = _at("2026-08-20T05:07:15.134811Z")
    return r


def fixture_bytes() -> bytes:
    with open(FIXTURE, "rb") as f:
        return f.read()


class CanonicalFormTest(unittest.TestCase):
    def test_encodes_to_the_agreed_bytes(self):
        """The point of a canonical form, tested the only way it can be: against
        the other two implementations' bytes."""
        self.assertEqual(fixture_bytes().decode(), fixture_record().to_json().decode())

    def test_the_agreed_bytes_round_trip_unchanged(self):
        """Decoding the agreed bytes and writing them straight back must not move
        a byte, or two implementations taking turns on one job churn the file
        against each other and no diff of its history means anything."""
        original = fixture_bytes()
        again = Record.from_json(original).to_json()
        self.assertEqual(original, again)
        rs = Record.from_json(original).checkpoint_ranges()
        self.assertEqual(len(rs), 3)
        self.assertEqual(verified_prefix(rs), 4194304)
        self.assertEqual(ranges_total(rs), 4194304 + 4194304 + 2097152)

    def test_merging_and_sorting(self):
        cases = [
            ("already canonical", [(0, 4), (8, 12)], [(0, 4), (8, 12)]),
            ("out of order", [(8, 12), (0, 4)], [(0, 4), (8, 12)]),
            # The case that makes the form canonical rather than merely sorted:
            # [[0,4],[4,8]] and [[0,8]] are the same proven bytes, so only one
            # of them may be legal or two implementations spell one state two
            # ways.
            ("adjacent merge", [(0, 4), (4, 8)], [(0, 8)]),
            ("overlapping merge", [(0, 6), (4, 8)], [(0, 8)]),
            ("contained", [(0, 8), (2, 4)], [(0, 8)]),
            ("identical", [(0, 8), (0, 8)], [(0, 8)]),
            ("chain", [(4, 8), (0, 4), (8, 9), (20, 21)], [(0, 9), (20, 21)]),
            # Proves nothing, so it is not part of the state and must not change
            # the bytes.
            ("empty range dropped", [(0, 4), (6, 6)], [(0, 4)]),
            ("all empty", [(6, 6)], []),
            ("nothing at all", [], []),
        ]
        for name, given, want in cases:
            with self.subTest(name):
                got = canonical_ranges(given)
                self.assertEqual(got, [Range(*w) for w in want])
                # Canonicalising a canonical set changes nothing, or one state
                # would have a spelling that depends on how many times it had
                # been written.
                self.assertEqual(canonical_ranges(got), got)

    def test_refuses_nonsense(self):
        for bad in ([(-1, 4)], [(0, -4)], [(8, 4)], [(0,)], [(0, 4, 8)], [("a", "b")]):
            with self.subTest(repr(bad)), self.assertRaises(Invalid):
                canonical_ranges(bad)


class VerifiedPrefixTest(unittest.TestCase):
    def test_is_the_range_starting_at_zero(self):
        cases = [
            ("one range at zero", [(0, 400)], 400),
            ("a range at zero and others", [(0, 400), (800, 900)], 400),
            # The case a single integer could never express, and the reason it
            # had to stop being the only thing a checkpoint says: real work is
            # proven and the prefix is still zero.
            ("nothing at zero", [(800, 900), (1000, 1200)], 0),
            ("empty set", [], 0),
            ("a gap closed by a merge", [(0, 400), (400, 800)], 800),
        ]
        for name, given, want in cases:
            with self.subTest(name):
                self.assertEqual(verified_prefix(given), want)
                # And what is written says the same thing as what is computed.
                self.assertEqual(
                    checkpoint_with_ranges(None, given)["verified_prefix"], want
                )

    def test_a_prefix_only_checkpoint_reads_as_one_range(self):
        """A checkpoint written before ranges existed IS a range set: one range
        starting at zero. Without this the degenerate case would be a special
        case and every caller would have to handle both."""
        self.assertEqual(
            ranges_from_checkpoint({"verified_prefix": 400}), [Range(0, 400)]
        )
        for empty in ({}, {"verified_prefix": 0}, None):
            with self.subTest(repr(empty)):
                self.assertEqual(ranges_from_checkpoint(empty), [])

    def test_a_staler_verified_set_does_not_lose_the_proven_prefix(self):
        """A prefix-only writer that took the job over and advanced the prefix
        without touching `verified` leaves a record where the two disagree. Both
        fields are claims that bytes are PROVEN and neither is a claim that other
        bytes are not, so the union is the only reading that loses nothing."""
        got = ranges_from_checkpoint(
            {"verified_prefix": 8388608, "verified": [[0, 4194304], [16777216, 20971520]]}
        )
        self.assertEqual(got, [Range(0, 8388608), Range(16777216, 20971520)])

    def test_a_checkpoint_that_is_not_ranges_is_refused(self):
        for bad in (
            {"verified": [[0]]},
            {"verified": [[0, 4, 8]]},
            {"verified": [[4, 0]]},
            {"verified": [[-1, 4]]},
            {"verified": "nope"},
            {"verified_prefix": "400"},
        ):
            with self.subTest(repr(bad)), self.assertRaises(Invalid):
                ranges_from_checkpoint(bad)


class AdditiveTest(unittest.TestCase):
    def test_a_reader_that_ignores_ranges_still_resumes_from_the_prefix(self):
        """The half of the change that makes it additive rather than a break.

        A reader that has never heard of `verified` decodes the checkpoint it
        does know, finds the prefix, and resumes from it. It re-fetches
        everything past the first gap, which is what it does today; it does not
        fail, and it does not trust a byte nobody proved.
        """
        r = Record.from_json(fixture_bytes())
        # Exactly what a pre-ranges reader does: one key, and no idea the other
        # one is there.
        self.assertEqual(r.checkpoint["verified_prefix"], 4194304)
        # And it is never told it must understand ranges before it may proceed.
        self.assertNotIn(MODEL_RANGES, r.critical)
        self.assertIn(MODEL_RANGES, r.content)

    def test_ranges_are_never_critical_even_if_a_writer_says_so(self):
        r = fixture_record()
        r.critical = list(r.critical) + [MODEL_RANGES]
        back = json.loads(r.to_json())
        self.assertNotIn(MODEL_RANGES, back["critical"])


class ParallelFetcherTest(unittest.TestCase):
    def sample(self, total: int) -> Record:
        return Record(id="", kind="download", spec={"bytes": total})

    def test_parts_landing_out_of_order_accumulate(self):
        r = self.sample(48)
        for part in (5, 0, 2, 1, 3, 4):
            r.add_checkpoint_range(part * 8, part * 8 + 8)
        # Six touching parts are one proven range, and the prefix is the file.
        self.assertEqual(r.checkpoint_ranges(), [Range(0, 48)])
        self.assertEqual(verified_prefix(r.checkpoint_ranges()), 48)

    def test_the_state_that_had_no_representation(self):
        """Parts 0, 2 and 5 done and the rest not: the sentence that could not be
        written down at all when a checkpoint was one integer."""
        r = self.sample(48)
        for part in (0, 2, 5):
            r.add_checkpoint_range(part * 8, part * 8 + 8)
        rs = r.checkpoint_ranges()
        self.assertEqual(rs, [Range(0, 8), Range(16, 24), Range(40, 48)])
        self.assertEqual(verified_prefix(rs), 8)
        # The gaps are what is left to fetch, which is the question a resume asks.
        self.assertEqual(ranges_missing(rs, 0, 48), [Range(8, 16), Range(24, 40)])

    def test_covers_and_missing(self):
        rs = [(0, 8), (16, 24)]
        for start, end, want in [
            (0, 8, True), (2, 6, True), (0, 9, False), (8, 16, False),
            (16, 24, True), (4, 4, True), (100, 100, True),
        ]:
            with self.subTest(f"{start},{end}"):
                self.assertEqual(ranges_cover(rs, start, end), want)
        self.assertEqual(ranges_missing(rs, 0, 32), [Range(8, 16), Range(24, 32)])
        self.assertEqual(ranges_missing([], 0, 32), [Range(0, 32)])
        self.assertEqual(ranges_missing(rs, 0, 4), [])


class CheckpointKeysTest(unittest.TestCase):
    def test_other_keys_survive_and_are_ordered(self):
        """A checkpoint belongs to whoever writes it. These helpers own two keys
        and must leave the rest where they were, sorted so all three
        implementations put them in the same order."""
        got = checkpoint_with_ranges(
            {"zebra": 1, "apple": {"nested": [1, 2]}, "verified_prefix": 99},
            [(0, 400)],
        )
        self.assertEqual(
            json.dumps(got, separators=(",", ":")),
            '{"verified_prefix":400,"verified":[[0,400]],"apple":{"nested":[1,2]},"zebra":1}',
        )

    def test_clearing_ranges_withdraws_the_declaration(self):
        r = fixture_record()
        r.clear_checkpoint_ranges()
        text = r.to_json().decode()
        self.assertNotIn(MODEL_RANGES, text)
        self.assertNotIn('"verified"', text)
        # The prefix survives, because it is not ours to remove: an old reader
        # still resumes from it.
        self.assertIn('"verified_prefix": 4194304', text)


class DeclarationSurvivesTest(unittest.TestCase):
    def test_a_read_modify_write_by_a_reader_that_ignores_it(self):
        """The declaration cannot be derived -- the checkpoint is opaque here --
        so it has to survive a reader that does a full read, modify and write
        without ever looking inside one."""
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        store = FileStore(d.name)

        r = fixture_record()
        r.id = ""
        r.state = "pending"
        r.lease.owner = ""
        r.lease.epoch = 0
        jid = store.submit(r)

        claimed = store.claim(jid, "somebody-else", 60)

        def touch(rec):
            rec.progress.done = 99  # nothing to do with ranges

        store.update(jid, claimed.lease.epoch, touch)

        got = store.load(jid)
        self.assertIn(MODEL_RANGES, got.content)
        self.assertEqual(len(got.checkpoint_ranges()), 3)


if __name__ == "__main__":
    unittest.main()
