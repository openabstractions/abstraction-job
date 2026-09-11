"""jobctl — the Python half of the cross-language conformance test.

Same commands as the Go jobctl, against the same directory. Neither knows the
other exists; both only know the record.
"""

import argparse
import json
import os
import platform
import sys
from datetime import datetime, timezone

try:
    import abstraction_config
except ImportError:
    sys.path.append(os.path.join(os.path.dirname(os.path.abspath(__file__)), os.pardir, os.pardir, "abstraction-config", "python"))
    import abstraction_config

from abstraction_job import (
    CANCEL,
    PAUSE,
    RUN,
    FileStore,
    JobError,
    Record,
    parse_opaque,
)


def store_root():
    """Resolve a store root rather than demanding one.

    JOB_STORE is jobctl's own override and stays here: the conformance harness
    points three implementations at one directory with it and must not inherit
    what the machine has configured. Every other rung is the config layer's
    answer, in one place, for all three languages.
    """
    value = os.environ.get("JOB_STORE")
    if value:
        return value, "JOB_STORE"
    try:
        return abstraction_config.job_store()
    except OSError as e:
        sys.exit("jobctl: %s\n  ABSTRACTION_STORE=…   names one" % e)


def store() -> FileStore:
    root, came_from = store_root()
    try:
        return FileStore(root)
    except OSError as e:
        sys.exit(
            "jobctl: %s: %s\n"
            "  that root came from %s\n"
            "  jobd setup --show     what this machine has configured, and which file said so\n"
            "  ABSTRACTION_STORE=…   names a store for one run" % (root, e, came_from)
        )


def main() -> None:
    # Windows text mode would turn every LF into CRLF, and a record printed here
    # is a conformance surface compared against Go and C++ byte for byte. Without
    # this the three implementations agree about every field and the comparison
    # still fails -- on line endings, which is the least interesting way to
    # disagree and the hardest to see in a diff.
    #
    # The encoding is here for the same reason. A record spells a filename in
    # raw UTF-8 [JOB-E6], and a Windows console defaults to the machine's code
    # page, so printing an accented name raised UnicodeEncodeError from a tool
    # that had read the record perfectly well.
    try:
        sys.stdout.reconfigure(newline="\n", encoding="utf-8")
    except AttributeError:  # pragma: no cover - Python < 3.7
        pass

    p = argparse.ArgumentParser(prog="jobctl.py")
    sub = p.add_subparsers(dest="cmd", required=True)

    s_submit = sub.add_parser("submit")
    s_submit.add_argument("--kind", required=True, help="what this job is; who can read the spec")
    s_submit.add_argument("--spec", required=True, help="the job's spec, as raw JSON")
    s_submit.add_argument("--total", type=int, default=0, help="total work, in the kind's own units")

    s_claim = sub.add_parser("claim")
    s_claim.add_argument("id")
    s_claim.add_argument("--owner", required=True)
    s_claim.add_argument("--ttl", type=float, default=30.0)

    s_prog = sub.add_parser("progress")
    s_prog.add_argument("id")
    s_prog.add_argument("--epoch", type=int, required=True)
    s_prog.add_argument("--done", type=int, default=0)
    s_prog.add_argument("--checkpoint", default="", help="what a successor needs to resume, as raw JSON")

    s_fin = sub.add_parser("finish")
    s_fin.add_argument("id")
    s_fin.add_argument("--epoch", type=int, required=True)
    s_fin.add_argument("--state", default="transferred")

    s_show = sub.add_parser("show")
    s_show.add_argument("id")

    # Say what should happen, without holding the lease. No --epoch, and that
    # absence is the feature: the caller is not the worker.
    s_int = sub.add_parser("intent")
    s_int.add_argument("id")
    s_int.add_argument("want", choices=[RUN, PAUSE, CANCEL])
    s_int.add_argument("--by", default="")

    # Ask the holder for the lease back. --epoch is the one the caller SAW, not
    # one it holds: a third party recalling a residency it has only read.
    s_rec = sub.add_parser("recall")
    s_rec.add_argument("id")
    s_rec.add_argument("--epoch", type=int, required=True)
    s_rec.add_argument("--reason", required=True)
    s_rec.add_argument("--grace", type=float, default=30.0)
    s_rec.add_argument("--by", default="")

    sub.add_parser("orphans")
    sub.add_parser("list")

    a = p.parse_args()
    st = store()

    try:
        if a.cmd == "submit":
            # Deliberately ignorant of what a job IS: the spec goes through
            # untouched, which is the same contract the module itself keeps.
            rec = Record(id="", kind=a.kind, spec=parse_opaque(a.spec))
            rec.progress.total = a.total
            print(st.submit(rec))

        elif a.cmd == "claim":
            r = st.claim(a.id, a.owner, a.ttl)
            cp = json.dumps(r.checkpoint, separators=(",", ":")) if r.checkpoint else "none"
            print(f"epoch={r.lease.epoch} state={r.state} checkpoint={cp}")

        elif a.cmd == "progress":
            def mutate(r):
                r.progress.done = a.done
                r.progress.updated_at = datetime.now(timezone.utc)
                if a.checkpoint:
                    r.checkpoint = parse_opaque(a.checkpoint)

            r = st.update(a.id, a.epoch, mutate)
            cp = json.dumps(r.checkpoint, separators=(",", ":")) if r.checkpoint else "none"
            print(f"done={r.progress.done} checkpoint={cp}")

        elif a.cmd == "finish":
            def mutate(r):
                r.state = a.state

            r = st.update(a.id, a.epoch, mutate)
            print(f"state={r.state}")

        elif a.cmd == "show":
            sys.stdout.write(st.load(a.id).to_json().decode())

        elif a.cmd == "intent":
            by = a.by or f"jobctl.py@{platform.node()}:{os.getpid()}"
            r = st.set_intent(a.id, a.want, by)
            print(f"{r.id} {r.wants()}")

        elif a.cmd == "recall":
            by = a.by or f"jobctl.py@{platform.node()}:{os.getpid()}"
            r = st.recall(a.id, a.epoch, a.reason, by, a.grace)
            print(f"{r.id} recalled until {r.lease.recall.until.isoformat()}")

        elif a.cmd == "orphans":
            for r in st.orphans():
                cp = json.dumps(r.checkpoint, separators=(",", ":")) if r.checkpoint else "none"
                print(f"{r.id} kind={r.kind} state={r.state} checkpoint={cp}")

        elif a.cmd == "list":
            for r in st.list():
                cp = json.dumps(r.checkpoint, separators=(",", ":")) if r.checkpoint else "none"
                print(
                    f"{r.id} kind={r.kind} state={r.state} "
                    f"done={r.progress.done} checkpoint={cp}"
                )

    except JobError as e:
        sys.exit(f"jobctl: {type(e).__name__}: {e}")


if __name__ == "__main__":
    main()
