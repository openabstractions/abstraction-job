"""job — a handle to work that is happening somewhere else.

This is the Python side of the same abstraction the Go package implements. The
two are NOT a port of one another and they are not generated from a shared IDL.
They are two independent implementations that agree about one thing: the record
on disk, and the rules for taking ownership of it.

That is deliberate, and it is the whole test. If a Go process can be killed
mid-job and a Python process can pick the job up and finish it, then the
abstraction is real. If the only way to make that work is for both sides to be
written by the same generator, it is a serialisation format, not an abstraction.

So the API here looks like Python — snake_case, dataclasses, exceptions — while
the bytes on disk are identical to what Go writes. What crosses the boundary is
the record. The method signatures are each language's own business.

WHAT THIS MODULE DOES NOT KNOW: what the work IS. `spec` and `checkpoint` are
opaque here and `kind` says who can read them. A download's artifact, sources
and destination live in a download's spec, not in this file — which is why
downloading can grow mirrors, chunk manifests and webseeds without changing the
record every language has to agree about.

Standard library only, on purpose: an abstraction that requires a dependency to
read a JSON file has misjudged its own weight.
"""

from __future__ import annotations

import binascii
import json
import os
import secrets
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Callable, Dict, List, Optional

SCHEMA_VERSION = 3
# History, kept because each bump cost something and the next one should have to
# justify itself against these:
#   1  first cut.
#   2  added Delegation. v1 assumed whoever held the lease was also doing the
#      work; Windows BITS takes the whole job, works under its own service
#      account while every process that asked is closed, and hands back only a
#      GUID. A v1 reader would have seen no progress and concluded it stalled.
#   3  moved artifact/sources/sink out into an opaque spec, and generalised
#      progress. Those were download concepts sitting in the generic layer, so
#      download could not evolve without changing everyone's schema. This is the
#      bump that is supposed to stop the bumps.

# States. The one that earns its place is TRANSFERRED: the work is finished and
# proven, but the result has not been taken delivery of. It exists because the
# process that does the work and the process that wants the result are not the
# same process — which is the entire premise.
PENDING = "pending"
RUNNING = "running"
TRANSFERRED = "transferred"
COMPLETE = "complete"
FAILED = "failed"
CANCELLED = "cancelled"

_STATES = {PENDING, RUNNING, TRANSFERRED, COMPLETE, FAILED, CANCELLED}
_TERMINAL = {COMPLETE, FAILED, CANCELLED}


class JobError(Exception):
    """Base for every refusal this module makes."""


class NotFound(JobError):
    pass


class Invalid(JobError):
    pass


class UnknownSchema(Invalid):
    pass


class LeaseHeld(JobError):
    """Someone else owns this job right now."""


class StaleEpoch(JobError):
    """The caller's epoch is not the record's epoch: it lost ownership."""


class LeaseExpired(JobError):
    """The caller's lease ran out. Re-claim; do not renew."""


class Terminal(JobError):
    pass


def _rfc3339(t: datetime) -> str:
    """Format the way Go's encoding/json writes time.Time.

    Go emits RFC 3339 with a 'Z' for UTC. Python's isoformat() emits '+00:00',
    which Go parses happily, but writing the same shape both ways keeps the files
    diffable and stops anyone concluding the two implementations disagree when
    they are looking at nothing but a timezone suffix.
    """
    t = t.astimezone(timezone.utc)
    s = t.isoformat(timespec="microseconds")
    return s[:-6] + "Z" if s.endswith("+00:00") else s


def _parse_time(s: str) -> datetime:
    if not s:
        return datetime.fromtimestamp(0, timezone.utc)
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    return datetime.fromisoformat(s)


@dataclass
class Progress:
    """Deliberately thin: two numbers and a timestamp, in units the kind defines.

    Best-effort and explicitly NOT monotonic — a job resuming from a checkpoint
    after a crash can legitimately report a smaller `done` than before. Nothing
    may make a decision on it. Anything richer belongs in the kind's checkpoint.
    """

    done: int = 0
    total: int = 0  # 0 means unknown
    updated_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))


@dataclass
class Lease:
    """The right to work on a job, for a bounded time.

    epoch is the part that matters: it rises by one on every claim, and every
    write must present the epoch it holds. A process that slept through its own
    expiry wakes up believing it is still the owner; its writes carry a stale
    epoch and are refused. Without that, two owners work one job and each
    believes the result is correct.
    """

    owner: str = ""
    epoch: int = 0
    expires_at: datetime = field(default_factory=lambda: datetime.fromtimestamp(0, timezone.utc))

    def held(self, now: datetime) -> bool:
        return bool(self.owner) and now < self.expires_at


@dataclass
class Delegation:
    """The work has been handed to something outside this process — a system
    service, a daemon on a NAS — which is now doing it.

    Not an accommodation for one tool; it is the architecture. An application's
    worker hands off to a system service when one is present, and that service
    hands off to the NAS when one is configured. The application never learns
    which did the work.

    When this is set, `progress` is a CACHE of what the external system last
    reported, not a measurement anyone here made. The external system is truth.
    """

    system: str = ""          # "bits", "nas" — who can interpret external_id
    external_id: str = ""     # that system's own handle; for BITS, a job GUID
    delivered: bool = False   # the delegate was told to hand the result over


@dataclass
class Record:
    """The whole job, and the cross-language contract.

    Everything a different process — in a different language, after a reboot —
    needs in order to continue this work has to be in here, because nothing else
    survives.
    """

    id: str
    kind: str = ""
    state: str = PENDING
    schema: int = SCHEMA_VERSION
    spec: Dict[str, Any] = field(default_factory=dict)
    checkpoint: Optional[Dict[str, Any]] = None
    progress: Progress = field(default_factory=Progress)
    lease: Lease = field(default_factory=Lease)
    delegation: Optional[Delegation] = None
    requires: List[str] = field(default_factory=list)
    error: str = ""
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    updated_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))

    # ---- the contract -----------------------------------------------------

    def validate(self) -> None:
        if self.schema != SCHEMA_VERSION:
            raise UnknownSchema(f"schema {self.schema}, understand {SCHEMA_VERSION}")
        if not self.id.strip():
            raise Invalid("id is required")
        if not self.kind.strip():
            raise Invalid("kind is required — an opaque spec nobody can identify is unusable")
        if self.state not in _STATES:
            raise Invalid(f"state {self.state!r}")
        if self.spec is None or not isinstance(self.spec, dict):
            raise Invalid("spec must be present and be a JSON object")
        if self.progress.done < 0 or self.progress.total < 0:
            raise Invalid("progress cannot be negative")
        if self.delegation is not None:
            if not self.delegation.system.strip() or not self.delegation.external_id.strip():
                raise Invalid("delegation needs both a system and an external id")

    def terminal(self) -> bool:
        return self.state in _TERMINAL

    def delegated(self) -> bool:
        """Something outside this process is doing the work.

        A caller finding this true must not start working itself: the external
        system does not participate in the lease, so two workers is exactly the
        damage the lease exists to prevent.
        """
        return self.delegation is not None

    def to_json(self) -> bytes:
        self.validate()
        # Key order and omissions match the Go struct tags exactly. Go decodes
        # with DisallowUnknownFields, so an extra key here is not a cosmetic
        # difference — it makes the record unreadable to the other half of the
        # abstraction.
        d: Dict[str, Any] = {
            "schema": self.schema,
            "id": self.id,
            "kind": self.kind,
            "state": self.state,
            "spec": self.spec,
        }
        if self.checkpoint is not None:
            d["checkpoint"] = self.checkpoint
        d["progress"] = {"done": self.progress.done}
        if self.progress.total:
            d["progress"]["total"] = self.progress.total
        d["progress"]["updated_at"] = _rfc3339(self.progress.updated_at)
        d["lease"] = {
            "owner": self.lease.owner,
            "epoch": self.lease.epoch,
            "expires_at": _rfc3339(self.lease.expires_at),
        }
        if self.delegation is not None:
            deleg: Dict[str, Any] = {
                "system": self.delegation.system,
                "external_id": self.delegation.external_id,
            }
            if self.delegation.delivered:
                deleg["delivered"] = True
            d["delegation"] = deleg
        if self.requires:
            d["requires"] = self.requires
        if self.error:
            d["error"] = self.error
        d["created_at"] = _rfc3339(self.created_at)
        d["updated_at"] = _rfc3339(self.updated_at)
        return (json.dumps(d, indent=2) + "\n").encode("utf-8")

    @staticmethod
    def from_json(b: bytes) -> "Record":
        try:
            d = json.loads(b)
        except ValueError as e:
            raise Invalid(str(e)) from None
        if d.get("schema") != SCHEMA_VERSION:
            raise UnknownSchema(f"schema {d.get('schema')}, understand {SCHEMA_VERSION}")
        p = d.get("progress") or {}
        l = d.get("lease") or {}
        dg = d.get("delegation")
        r = Record(
            id=d.get("id", ""),
            kind=d.get("kind", ""),
            state=d.get("state", ""),
            schema=d["schema"],
            spec=d.get("spec"),
            checkpoint=d.get("checkpoint"),
            progress=Progress(
                done=p.get("done", 0),
                total=p.get("total", 0),
                updated_at=_parse_time(p.get("updated_at", "")),
            ),
            lease=Lease(
                owner=l.get("owner", ""),
                epoch=l.get("epoch", 0),
                expires_at=_parse_time(l.get("expires_at", "")),
            ),
            delegation=(
                Delegation(
                    system=dg.get("system", ""),
                    external_id=dg.get("external_id", ""),
                    delivered=dg.get("delivered", False),
                )
                if dg
                else None
            ),
            requires=d.get("requires") or [],
            error=d.get("error", ""),
            created_at=_parse_time(d.get("created_at", "")),
            updated_at=_parse_time(d.get("updated_at", "")),
        )
        r.validate()
        return r


def new_id() -> str:
    """An id that sorts by creation time, so a directory listing is in roughly
    submission order without anything having to record that separately."""
    return f"{int(time.time() * 1000)}-{binascii.hexlify(secrets.token_bytes(10)).decode()}"


class FileStore:
    """Jobs as files in a directory.

    A directory is the one thing a Go process, a Python process, a Windows
    service and a machine that has just rebooted can all agree on without any of
    them running at the same time. No daemon, no database, no socket.

        <root>/jobs/<id>.json        the record
        <root>/jobs/<id>.epoch.<n>   claim token for epoch n, created O_EXCL
        <root>/work/<id>             scratch space for a job that needs it

    The claim tokens are the mutual exclusion. Exclusive create is atomic on both
    NTFS and POSIX, so exactly one process can create `<id>.epoch.7`. Because
    each generation gets its own filename, a token left behind by a process that
    was killed blocks nothing — the next claimant takes epoch 8. A single
    lockfile would instead have to be broken by timeout, and breaking locks by
    timeout is how two owners end up working one job.
    """

    def __init__(self, root: str, now: Callable[[], datetime] = None):
        self.root = root
        self._now = now or (lambda: datetime.now(timezone.utc))
        os.makedirs(os.path.join(root, "jobs"), exist_ok=True)
        os.makedirs(os.path.join(root, "work"), exist_ok=True)

    # ---- paths ------------------------------------------------------------

    def _record_path(self, job_id: str) -> str:
        return os.path.join(self.root, "jobs", job_id + ".json")

    def _epoch_path(self, job_id: str, epoch: int) -> str:
        return os.path.join(self.root, "jobs", f"{job_id}.epoch.{epoch}")

    def work_path(self, job_id: str) -> str:
        """Scratch space a job may use while it runs. What goes there is the
        kind's business; the store only guarantees the path is derived from the
        id, so a successor can find what a predecessor left."""
        return os.path.join(self.root, "work", job_id)

    # ---- reading ----------------------------------------------------------

    def load(self, job_id: str) -> Record:
        """Any process may do this, including one that holds no lease and never
        will. That is what makes progress observable from outside — a callback
        cannot, because a callback is bound to the lifetime of the process that
        registered it, and that lifetime is the one that fails."""
        try:
            with open(self._record_path(job_id), "rb") as f:
                return Record.from_json(f.read())
        except FileNotFoundError:
            raise NotFound(job_id) from None

    def list(self) -> List[Record]:
        out = []
        d = os.path.join(self.root, "jobs")
        for name in sorted(os.listdir(d)):
            if not name.endswith(".json"):
                continue
            try:
                out.append(self.load(name[: -len(".json")]))
            except JobError:
                continue  # one unreadable record must not hide every other job
        return out

    def claimable(self, r: Record) -> bool:
        return not r.terminal() and not r.lease.held(self._now())

    def orphans(self) -> List[Record]:
        """The jobs nobody is working on.

        This is the primary reclamation path, not a fallback. A process that is
        SIGKILLed, or a machine that loses power, never gets to hand anything
        over — so a design that relies on graceful handoff has no answer for the
        case that actually loses a 40 GB download.
        """
        return [r for r in self.list() if self.claimable(r)]

    # ---- writing ----------------------------------------------------------

    def submit(self, r: Record) -> str:
        if not r.id:
            r.id = new_id()
        now = self._now()
        r.schema = SCHEMA_VERSION
        if not r.state:
            r.state = PENDING
        r.created_at = now
        r.updated_at = now
        r.progress.updated_at = now
        data = r.to_json()
        # Exclusive: submitting the same id twice is a caller bug, not something
        # to paper over by overwriting a job that may be running right now.
        fd = os.open(self._record_path(r.id), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
        with os.fdopen(fd, "wb") as f:
            f.write(data)
        return r.id

    def claim(self, job_id: str, owner: str, ttl_seconds: float) -> Record:
        """Take ownership for ttl_seconds, returning the record with the caller's
        new epoch. Every later write must present that epoch."""
        if not owner.strip():
            raise JobError("claim requires an owner")
        r = self.load(job_id)
        if r.terminal():
            raise Terminal(f"{job_id} is {r.state}")
        now = self._now()
        if r.lease.held(now) and r.lease.owner != owner:
            raise LeaseHeld(f"{r.lease.owner} holds it until {_rfc3339(r.lease.expires_at)}")

        nxt = r.lease.epoch + 1
        try:
            fd = os.open(self._epoch_path(job_id, nxt), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
        except FileExistsError:
            raise LeaseHeld(f"epoch {nxt} was taken by someone else") from None
        with os.fdopen(fd, "w") as f:
            f.write(owner + "\n")

        r.lease = Lease(
            owner=owner,
            epoch=nxt,
            expires_at=datetime.fromtimestamp(now.timestamp() + ttl_seconds, timezone.utc),
        )
        if r.state in (PENDING, RUNNING):
            r.state = RUNNING
        self._write(r)
        return r

    def renew(self, job_id: str, epoch: int, ttl_seconds: float) -> Record:
        """Extend a lease the caller still holds.

        Refuses once expired even when the epoch still matches and nobody else
        has claimed. That is the sleep case: a process suspended for an hour
        wakes believing it is still the owner. Forcing it to re-claim bumps the
        epoch, so anything it had in flight is refused rather than landing on top
        of work a different owner may since have done.
        """
        r = self.load(job_id)
        if r.lease.epoch != epoch:
            raise StaleEpoch(f"record is at epoch {r.lease.epoch}, caller holds {epoch}")
        if not r.lease.held(self._now()):
            raise LeaseExpired(f"expired at {_rfc3339(r.lease.expires_at)}, re-claim instead")
        now = self._now()
        r.lease.expires_at = datetime.fromtimestamp(now.timestamp() + ttl_seconds, timezone.utc)
        self._write(r)
        return r

    def release(self, job_id: str, epoch: int) -> None:
        """Give up a lease early so the job can be taken immediately. The
        graceful path, and only a courtesy: everything still works without it,
        just more slowly."""

        def mutate(r: Record) -> None:
            r.lease.expires_at = self._now()
            r.lease.owner = ""
            if r.state == RUNNING:
                r.state = PENDING

        self.update(job_id, epoch, mutate)

    def update(self, job_id: str, epoch: int, mutate: Callable[[Record], None]) -> Record:
        """The single gate every change passes through, so staleness is checked
        in exactly one place instead of once per call site."""
        r = self.load(job_id)
        if r.lease.epoch != epoch:
            raise StaleEpoch(f"record is at epoch {r.lease.epoch}, caller holds {epoch}")
        if not r.lease.held(self._now()):
            raise LeaseExpired(f"expired at {_rfc3339(r.lease.expires_at)}")
        mutate(r)
        r.updated_at = self._now()
        self._write(r)
        return r

    def _write(self, r: Record) -> None:
        """Replace the record atomically. A reader opening the file at any moment
        sees the old record or the new one, never half of one — and one of those
        readers may be deciding right now whether this job is an orphan."""
        data = r.to_json()
        d = os.path.join(self.root, "jobs")
        tmp = os.path.join(d, f"{r.id}.tmp-{os.getpid()}-{secrets.token_hex(4)}")
        with open(tmp, "wb") as f:
            f.write(data)
        os.replace(tmp, self._record_path(r.id))
