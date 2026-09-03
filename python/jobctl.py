"""jobctl — the Python half of the cross-language conformance test.

Same commands as the Go jobctl, against the same directory. Neither knows the
other exists; both only know the record.
"""

import argparse
import os
import platform
import sys
from datetime import datetime, timezone

import json

from abstraction_job import CANCEL, PAUSE, RUN, FileStore, JobError, Record


def store() -> FileStore:
    root = os.environ.get("JOB_STORE")
    if not root:
        sys.exit("jobctl: JOB_STORE is not set")
    return FileStore(root)


def main() -> None:
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

    sub.add_parser("orphans")

    a = p.parse_args()
    st = store()

    try:
        if a.cmd == "submit":
            # Deliberately ignorant of what a job IS: the spec goes through
            # untouched, which is the same contract the module itself keeps.
            rec = Record(id="", kind=a.kind, spec=json.loads(a.spec))
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
                    r.checkpoint = json.loads(a.checkpoint)

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

        elif a.cmd == "orphans":
            for r in st.orphans():
                cp = json.dumps(r.checkpoint, separators=(",", ":")) if r.checkpoint else "none"
                print(f"{r.id} kind={r.kind} state={r.state} checkpoint={cp}")

    except JobError as e:
        sys.exit(f"jobctl: {type(e).__name__}: {e}")


if __name__ == "__main__":
    main()
