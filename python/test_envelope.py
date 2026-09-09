"""The base envelope, read by the second implementation.

The corpus under ``job/testdata/envelope`` is the point of this file. The rules
the envelope adds -- the schema grammar, and that every action name resolves to
a schema the record declares -- are reader obligations, which the definition
cannot state and the generated codec therefore cannot enforce. That is the exact
class of rule where three hand-written readers drift silently, so the expected
verdict lives in one directory and every language reads it.
"""

import os
import socket
import sys
import tempfile
import threading
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "cas", "python"))

from abstraction_job import (  # noqa: E402
    ACTION_CANCEL,
    ACTION_PAUSE,
    BASE_SCHEMA,
    FEATURE_ENVELOPE,
    Envelope,
    FileStore,
    Invalid,
    MemoryStore,
    NotSupported,
    Record,
    Supervisor,
    UnknownSchema,
    split_action,
    valid_schema,
)

CORPUS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "testdata", "envelope")
VENDOR = "nas.example/transfer@2"


def _corpus(name):
    with open(os.path.join(CORPUS, name), "rb") as f:
        return f.read()


def _verdict(raw):
    try:
        return "accept", Record.from_json(raw)
    except UnknownSchema:
        return "unknown_schema", None
    except Invalid:
        return "invalid", None


def _ask_word(r, action, supervisor):
    try:
        r.ask(action, supervisor)
        return "ok"
    except UnknownSchema:
        return "unknown_schema"
    except NotSupported:
        return "not_supported"
    except Invalid:
        return "invalid"


class EnvelopeCorpus(unittest.TestCase):
    def test_every_case_gets_the_verdict_its_name_declares(self):
        names = sorted(n for n in os.listdir(CORPUS) if n.endswith(".json"))
        self.assertTrue(names, "no corpus")
        for name in names:
            raw = _corpus(name)
            want = name.split(".", 1)[0]
            got, record = _verdict(raw)
            self.assertEqual(want, got, name)
            if want != "accept":
                continue
            # An accepted case is also the byte proof: the file IS the canonical
            # form, so a reader that re-spells anything on the way out fails
            # here rather than in a diff between two languages a week later.
            self.assertEqual(raw, record.to_json(), name)

    def test_the_ask_corpus(self):
        path = os.path.join(CORPUS, "asks.tsv")
        seen = 0
        with open(path, encoding="utf-8") as f:
            for n, line in enumerate(f, 1):
                line = line.rstrip("\n")
                if not line or line.startswith("#"):
                    continue
                col = line.split("\t")
                self.assertEqual(5, len(col), f"asks.tsv:{n}")
                r = Record.from_json(_corpus(col[0]))
                s = Supervisor(
                    schemas=[] if col[2] == "-" else col[2].split(),
                    actions=[] if col[3] == "-" else col[3].split(),
                )
                self.assertEqual(col[4], _ask_word(r, col[1], s), f"asks.tsv:{n} {col[0]} {col[1]}")
                seen += 1
        self.assertTrue(seen)


class SchemaIsAName(unittest.TestCase):
    def test_an_identifier_cannot_be_an_address(self):
        for s in (
            "https://example.invalid/steal@1",
            "http://127.0.0.1:8080/x@1",
            "file:///etc/passwd@1",
            "//example.invalid/x@1",
            "\\\\host\\share\\x@1",
            "../../../etc/passwd@1",
            "C:/windows/system32@1",
            "example.invalid/%2e%2e/x@1",
            "example.invalid/x@1#fragment",
            "example.invalid/a/b@1",
            "Example.Invalid/X@1",
            "example.invalid/x",
            "example.invalid/x@0",
            "example.invalid/x@01",
            "example..invalid/x@1",
            "example invalid/x@1",
            "",
            "a" * 130 + "/x@1",
        ):
            self.assertFalse(valid_schema(s), s)
        for s in ("abstraction.job/base@1", VENDOR, "a/b@1", "a-b.c-d.e/f-g@123456789"):
            self.assertTrue(valid_schema(s), s)

    def test_nothing_dereferences_an_identifier(self):
        """A listener nobody is allowed to reach, its own address written into
        the record in every disguise, and a count of what arrived."""
        ln = socket.socket()
        ln.bind(("127.0.0.1", 0))
        ln.listen(5)
        addr = "%s:%d" % ln.getsockname()
        reached = []

        def accept():
            while True:
                try:
                    c, _ = ln.accept()
                except OSError:
                    return
                reached.append(1)
                c.close()

        t = threading.Thread(target=accept)
        t.start()
        try:
            for s in ("http://" + addr + "/schema@1", "//" + addr + "/schema@1", addr + "/schema@1"):
                r = _record(Envelope(schema=s))
                with self.assertRaises(Invalid):
                    r.to_json()
            r = _record(Envelope(schema=VENDOR, actions=[ACTION_PAUSE, VENDOR + "#verify"]))
            back = Record.from_json(r.to_json())
            back.supports(VENDOR + "#verify")
            _ask_word(back, VENDOR + "#verify", Supervisor([VENDOR], [VENDOR + "#verify"]))
        finally:
            ln.close()
            t.join()
        self.assertEqual([], reached, "reading a record opened a connection to an address inside it")


def _record(envelope):
    r = Record(id="1787202430967-a752f9a9c2c77b123ffd", kind="test")
    r.spec = {"anything": 1}
    r.envelope = envelope
    return r


class ActionsResolve(unittest.TestCase):
    def test_a_namespaced_action_does_not_collide_with_a_bare_one(self):
        r = _record(Envelope(schema=VENDOR, actions=[VENDOR + "#cancel"]))
        self.assertFalse(r.supports(ACTION_CANCEL))
        self.assertTrue(r.supports(VENDOR + "#cancel"))
        both = _record(Envelope(schema=VENDOR, actions=[ACTION_CANCEL, VENDOR + "#cancel"]))
        both.to_json()
        self.assertTrue(both.supports(ACTION_CANCEL) and both.supports(VENDOR + "#cancel"))

    def test_every_action_resolves_to_a_declared_schema(self):
        for actions in (
            ["remirror"],
            [BASE_SCHEMA + "#pause"],
            ["someone.else/other@1#verify"],
            [ACTION_PAUSE, ACTION_PAUSE],
            ["Verify"],
            ["https://evil.invalid/x@1#go"],
            [VENDOR + "#a#b"],
        ):
            with self.assertRaises(Invalid, msg=str(actions)):
                _record(Envelope(schema=VENDOR, actions=actions)).to_json()
        _record(Envelope(schema=VENDOR, actions=["pause", "resume", "cancel", "recall",
                                                 VENDOR + "#verify"])).to_json()

    def test_split(self):
        self.assertEqual(("", "pause", True), split_action("pause"))
        self.assertEqual((VENDOR, "verify", True), split_action(VENDOR + "#verify"))
        self.assertFalse(split_action("a#b#c")[2])


class EnvelopeIsAPropertyOfTheKind(unittest.TestCase):
    def test_a_lease_holder_cannot_move_it(self):
        with tempfile.TemporaryDirectory() as root:
            for store in (FileStore(root), MemoryStore()):
                r = _record(Envelope(schema=VENDOR, actions=[ACTION_PAUSE]))
                job_id = store.submit(r)
                held = store.claim(job_id, "a-holder", 60)

                def moved(rr):
                    rr.envelope.actions = [ACTION_PAUSE, VENDOR + "#verify"]

                def dropped(rr):
                    rr.envelope = None

                def renamed(rr):
                    rr.envelope.schema = "someone.else/other@1"

                for change in (moved, dropped, renamed):
                    with self.assertRaises(Invalid):
                        store.update(job_id, held.lease.epoch, change)
                got = store.load(job_id)
                self.assertEqual(VENDOR, got.schema())
                self.assertEqual([ACTION_PAUSE], got.actions())

                def ordinary(rr):
                    rr.progress.done = 7

                store.update(job_id, held.lease.epoch, ordinary)

    def test_an_envelope_is_declared_and_critical(self):
        r = _record(Envelope(schema=VENDOR))
        r.to_json()
        self.assertIn(FEATURE_ENVELOPE, r.content)
        self.assertIn(FEATURE_ENVELOPE, r.critical)
        plain = _record(None)
        plain.to_json()
        self.assertNotIn(FEATURE_ENVELOPE, plain.content)


if __name__ == "__main__":
    unittest.main()
