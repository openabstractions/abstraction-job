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
import tempfile
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Callable, Dict, List, Optional, Protocol, runtime_checkable

# The data models this implementation understands, and writes.
#
# A name is namespaced and versioned: "abstraction.job/base@1". The version is
# part of the name rather than a separate field, so an incompatible change to one
# model is a NEW name that old readers correctly fail to recognise, while every
# other model in the record stays readable.
#
# This replaced an integer version. A version conflates WHAT CHANGED with WHAT
# YOU MUST UNDERSTAND, so the only safe response to any unknown version was to
# refuse the whole record -- even when the addition was decoration a reader could
# have ignored. See CRITICAL below for the half that makes this more than a
# rename.
MODEL_BASE = "abstraction.job/base@1"
MODEL_INTENT = "abstraction.job/intent@1"
MODEL_DELEGATION = "abstraction.job/delegation@1"
MODEL_STEP = "abstraction.job/step@1"
# A checkpoint carrying proven byte ranges rather than only a prefix. NEVER
# critical -- see the range section below for why that is the half of the design
# that makes it an addition rather than a break.
MODEL_RANGES = "abstraction.download/ranges@1"

# What this implementation can read. A record naming anything else in `critical`
# is refused.
KNOWN_MODELS = frozenset(
    {MODEL_BASE, MODEL_INTENT, MODEL_DELEGATION, MODEL_STEP, MODEL_RANGES}
)

# Models this layer declares but cannot derive, because what they describe lives
# inside a field that is opaque here.
#
# Everything else in `content` is rediscovered on every write, so a declaration
# cannot drift from the data. A model describing the CHECKPOINT's contents
# cannot be: working it out would mean reading the checkpoint, and this module
# does not know what a checkpoint is. So the declaration is carried instead --
# the same treatment an unknown extension gets, for the same reason.
CARRIED_MODELS = frozenset({MODEL_RANGES})

# Models that must not appear in `critical` whoever asked for it. Both are
# advisory by their own definition, so marking one critical tells a stranger to
# refuse work over a decoration. This layer will not relay that.
NEVER_CRITICAL = frozenset({MODEL_STEP, MODEL_RANGES})

# The integer this format used to carry, mapped onto the models each version
# implied. No longer written; still read, because stores full of version 3 and 4
# records exist on real disks and on a NAS. The mapping is exact rather than a
# guess: those versions are frozen and it is known what each could contain.
LEGACY_SCHEMAS = {
    3: (MODEL_BASE,),
    4: (MODEL_BASE, MODEL_INTENT),
    5: (MODEL_BASE, MODEL_INTENT, MODEL_STEP),
}


def understands(critical) -> tuple:
    """The first critical model this implementation cannot read, and whether it
    can proceed.

    Only `critical` is consulted. `content` is informational: a name there that
    nobody here knows is data to carry, not a reason to stop.
    """
    for name in critical or ():
        if name not in KNOWN_MODELS:
            return name, False
    return "", True


# What somebody wants to happen, as against what is happening.
RUN = "run"
PAUSE = "pause"
CANCEL = "cancel"
_WANTS = (RUN, PAUSE, CANCEL)
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


# ---------------------------------------------------------------------------
# A checkpoint of ranges.
#
# WHAT ONE INTEGER COULD NOT SAY. A checkpoint used to hold a single verified
# prefix: "the first N bytes are proven", and nothing else. Every transfer that
# could be described was therefore one stream appending to the end of a file.
# That is not how bytes are fetched -- sixteen concurrent ranged parts landing at
# scattered offsets in a sparse file is ordinary -- and "parts 0, 2 and 5 done,
# 1, 3 and 4 partway" had no representation at all. An adopter with a parallel
# fetcher could only take this library by deleting its parallelism.
#
# A range set says it. The prefix is the degenerate case: one range at zero.
#
#     "checkpoint": {
#       "verified_prefix": 4194304,
#       "verified": [[0, 4194304], [8388608, 12582912]]
#     }
#
# WHY THE PREFIX STAYS. Not redundancy and not politeness. A reader that has
# never heard of `verified` resumes from `verified_prefix` and re-fetches the
# rest, which is exactly what it does today. That is the whole reason this is an
# addition rather than a break, and it is why MODEL_RANGES is never critical: an
# old reader ignoring the ranges loses some bytes to a second fetch, and marking
# it critical would stop every existing reader dead for no safety gain. The
# prefix is DERIVED from the set on write, so nothing has to remember to keep
# the two agreeing.
#
# WHAT A RANGE MEANS. Proven, not merely written: the bytes are on disk AND
# checked, against a piece digest where the kind's spec carries one and against
# the transport's own framing where it does not. Bytes in flight when a process
# is killed are exactly the ones a successor must not trust; that rule is
# unchanged, and now applies per range instead of to one tail.
#
# WHERE THIS SITS. A checkpoint is opaque to this module. These helpers do not
# change that -- they are a canonical FORM offered to whoever writes one, not a
# meaning read out of every record relayed. `to_json` leaves a checkpoint
# exactly as it found it; only a caller that asks for ranges gets them
# rewritten.

VERIFIED_PREFIX_KEY = "verified_prefix"
VERIFIED_KEY = "verified"


class Range(tuple):
    """A half-open byte interval: `start` is included, `end` is not.

    A tuple, so a caller may hand in plain ``(0, 400)`` pairs and get pairs
    back, and so equality means what it looks like it means.
    """

    __slots__ = ()

    def __new__(cls, start, end):
        return super().__new__(cls, (int(start), int(end)))

    @property
    def start(self) -> int:
        return self[0]

    @property
    def end(self) -> int:
        return self[1]

    @property
    def size(self) -> int:
        return self[1] - self[0]

    def __repr__(self) -> str:
        return f"[{self[0]},{self[1]})"


def canonical_ranges(ranges) -> List[Range]:
    """Sort, merge and validate a set of ranges: the merge-on-write the format
    promises.

    Callers hand in whatever they have -- out of order, overlapping, duplicated,
    adjacent -- and get the one spelling of that state every implementation
    agrees on.

    ADJACENT ranges merge as well as overlapping ones, and that is not
    tidiness. ``[[0,4],[4,8]]`` and ``[[0,8]]`` are the same proven bytes; if
    both were legal, two implementations could write one state as different
    bytes and a conformance test comparing files would call them a
    disagreement. Merging touching ranges is what makes the form canonical
    rather than merely sorted.
    """
    kept = []
    for r in ranges or ():
        try:
            start, end = r
        except (TypeError, ValueError):
            raise Invalid(f"a verified range is a pair [start, end), got {r!r}") from None
        if not isinstance(start, int) or not isinstance(end, int) or isinstance(start, bool) or isinstance(end, bool):
            raise Invalid(f"a byte offset is a whole number, got {r!r}")
        if start < 0 or end < 0:
            raise Invalid(f"a byte offset cannot be negative: [{start},{end})")
        if end < start:
            raise Invalid(f"a range ends before it starts: [{start},{end})")
        # An empty range is not an error -- a fetcher that recorded a
        # zero-length part is not lying, it has just proven nothing -- but it
        # carries no information, and two sets differing only by one are the
        # same set.
        if end == start:
            continue
        kept.append(Range(start, end))
    kept.sort()

    out: List[Range] = []
    for r in kept:
        if out and r.start <= out[-1].end:
            if r.end > out[-1].end:
                out[-1] = Range(out[-1].start, r.end)
            continue
        out.append(r)
    return out


def verified_prefix(ranges) -> int:
    """The end of the range starting at zero, or 0 when there is none.

    In a canonical set at most one range can start at zero and it is the first,
    so this is the whole rule.
    """
    rs = canonical_ranges(ranges)
    return rs[0].end if rs and rs[0].start == 0 else 0


def ranges_total(ranges) -> int:
    """How many proven bytes the set holds."""
    return sum(r[1] - r[0] for r in canonical_ranges(ranges))


def ranges_cover(ranges, start: int, end: int) -> bool:
    """Whether every byte of [start, end) is proven. An empty interval is
    covered by anything, including the empty set."""
    if end <= start:
        return True
    return any(r[0] <= start and end <= r[1] for r in canonical_ranges(ranges))


def ranges_missing(ranges, start: int, end: int) -> List[Range]:
    """The gaps in [start, end) that are not proven yet -- what a fetcher still
    has to ask for, which is the question a resume asks."""
    out: List[Range] = []
    at = start
    for r in canonical_ranges(ranges):
        if r.end <= at:
            continue
        if r.start >= end:
            break
        if r.start > at:
            out.append(Range(at, r.start))
        at = max(at, r.end)
        if at >= end:
            break
    if at < end:
        out.append(Range(at, end))
    return out


def ranges_from_checkpoint(checkpoint) -> List[Range]:
    """The proven ranges a checkpoint carries.

    Three inputs, one answer:

      * ``verified`` present: those ranges, canonicalised.
      * only ``verified_prefix``: ``[(0, prefix)]``, because a prefix IS a
        range. This is what lets a record written before ranges existed be read
        as one, and what makes "the prefix is the degenerate case" true in code
        rather than only in prose.
      * neither, or no checkpoint at all: the empty set.

    When both are present the prefix is UNIONED IN rather than checked against
    the ranges. A prefix-only writer that took the job over and advanced the
    prefix without touching ``verified`` left a record where the two disagree,
    and the union is the only reading that loses nothing: both fields are claims
    that bytes are proven, and neither is a claim that other bytes are not.
    """
    if not checkpoint:
        return []
    if not isinstance(checkpoint, dict):
        raise Invalid("a checkpoint carrying ranges must be a JSON object")
    found = []
    raw = checkpoint.get(VERIFIED_KEY)
    if raw is not None:
        if not isinstance(raw, (list, tuple)):
            raise Invalid(f"{VERIFIED_KEY!r} is a list of [start, end) pairs")
        for pair in raw:
            if not isinstance(pair, (list, tuple)) or len(pair) != 2:
                raise Invalid(f"a verified range is a pair [start, end), got {pair!r}")
            found.append((pair[0], pair[1]))
    prefix = checkpoint.get(VERIFIED_PREFIX_KEY)
    if prefix is not None:
        if not isinstance(prefix, int) or isinstance(prefix, bool):
            raise Invalid(f"{VERIFIED_PREFIX_KEY!r} is a whole number of bytes")
        if prefix > 0:
            found.append((0, prefix))
    return canonical_ranges(found)


def checkpoint_with_ranges(checkpoint, ranges) -> Dict[str, Any]:
    """A checkpoint carrying `ranges` canonically, keeping every key it does not
    own.

    The form is pinned, because three implementations have to produce the same
    bytes for the same state::

        {"verified_prefix": P, "verified": [[s, e], ...], <everything else, by key>}

    The two range keys come first and in that order -- a reader skimming a
    record should see the number that matters first -- and the caller's other
    keys follow sorted by name. Sorted rather than left as found because Go
    reaches a checkpoint through a map, which has no order to preserve, so "as
    found" is not something all three languages can agree to do.
    """
    if checkpoint is not None and not isinstance(checkpoint, dict):
        raise Invalid("a checkpoint carrying ranges must be a JSON object")
    canon = canonical_ranges(ranges)
    out: Dict[str, Any] = {
        VERIFIED_PREFIX_KEY: verified_prefix(canon),
        VERIFIED_KEY: [[r.start, r.end] for r in canon],
    }
    for name in sorted(checkpoint or {}):
        if name in (VERIFIED_PREFIX_KEY, VERIFIED_KEY):
            continue
        out[name] = checkpoint[name]
    return out




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
class Step:
    """Which phase of a multi-phase job is happening now.

    In the record and not the kind's checkpoint, because a checkpoint is opaque:
    only a reader that knows the kind could render it. Progress is the one thing
    a GENERIC reader has to be able to display — a supervisor's status output, a
    download manager listing work of every kind — without knowing what the work
    is.

    What made it necessary: a download delegated to a NAS is two transfers, and
    only the first was visible. The far side fetched 40 GB and reported done;
    then this machine copied those 40 GB back across a share and re-hashed them,
    with the record still showing the first transfer's numbers throughout. A
    person watching saw a finished download doing nothing, for minutes, twice.

    ADVISORY ONLY, which is the same rule progress already has. Nothing may
    decide anything on a step — not what to do next, not whether work is
    finished, not whether to retry. The moment something branches on
    ``ordinal == 2``, a workflow engine has been smuggled into a record whose
    whole value is that it describes work without prescribing it.

    ``name`` is opaque to this layer, exactly like kind and spec.
    """

    name: str = ""
    ordinal: int = 0  # counts from one
    of: int = 0  # 0 when the writer cannot say how many
    # This phase's own units, which need not be the job's: hashing counts the
    # same bytes a second time, and the overall numbers must not double for it.
    done: int = 0
    total: int = 0


@dataclass
class Progress:
    """Deliberately thin: two numbers and a timestamp, in units the kind defines.

    Best-effort and explicitly NOT monotonic — a job resuming from a checkpoint
    after a crash can legitimately report a smaller `done` than before. Nothing
    may make a decision on it. Anything richer belongs in the kind's checkpoint,
    with the one exception of `step` — see Step for why that exception exists.
    """

    done: int = 0
    total: int = 0  # 0 means unknown
    updated_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    # Absent means what every record before schema 5 meant: one unnamed phase.
    step: Optional["Step"] = None


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
class Intent:
    """The desired state, separate from the observed one.

    Why this exists, which is not "so there can be a pause button": every write
    to a record needs the lease, and whoever wants a change is almost never
    holding it — a person clicks cancel in an application while a service on
    another machine moves the bytes. Before this field that was inexpressible,
    and the only alternative was to steal the job, which is the single thing the
    lease exists to prevent.

    Every system that has met this problem separates desired from observed:
    Kubernetes has spec against status with a deletionTimestamp anyone may set,
    Temporal records cancellation-requested apart from the run state, systemd
    distinguishes wanted from active, BITS exposes a state its own service polls.

    The rules, which are the contract rather than this implementation's habits:

      1. Anyone may write it, lease or no lease. It is the ONE field exempt.
      2. Only the lease holder may write `state`. Unchanged.
      3. An owner MUST check it at least as often as it checkpoints and move
         toward it. An owner that reads a record and ignores this is not an
         implementation of this abstraction.
      4. CANCEL must be honoured by everything; stopping is universal.
      5. PAUSE must be honoured by implementations that advertise it. One that
         cannot must FAIL the job with a reason rather than carry on, because a
         pause that quietly does nothing is worse than a refusal.
      6. A paused job is NOT an orphan — see `orphans`.
      7. Once `state` is terminal this is history.
    """

    want: str = RUN
    # Who asked. Not decoration: a job sitting against somebody's wish is one of
    # the few things that cannot be worked out from outside, and "which process
    # asked for this" is the first question anyone has.
    by: str = ""
    at: Optional[datetime] = None


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
    # What this record carries, and the subset a reader MUST understand or
    # refuse it entirely. See MODEL_BASE and the `critical` rule: not knowing
    # the step model is harmless, not knowing the intent model means working on
    # a job somebody asked to stop.
    content: List[str] = field(default_factory=list)
    critical: List[str] = field(default_factory=list)
    # Data this layer does not understand, keyed by a name that says who does.
    # A reader that cannot read one MUST preserve it on write: dropping it
    # destroys another participant's data, invisibly, because nobody here can
    # see what was lost.
    extensions: Dict[str, Any] = field(default_factory=dict)
    spec: Dict[str, Any] = field(default_factory=dict)
    checkpoint: Optional[Dict[str, Any]] = None
    progress: Progress = field(default_factory=Progress)
    lease: Lease = field(default_factory=Lease)
    delegation: Optional[Delegation] = None
    requires: List[str] = field(default_factory=list)
    error: str = ""
    # What somebody WANTS to happen, as against state, which is what IS
    # happening. Absent means run. Anyone may write it, lease or no lease;
    # the owner honours it. See Intent.
    intent: Optional["Intent"] = None
    created_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))
    updated_at: datetime = field(default_factory=lambda: datetime.now(timezone.utc))

    # ---- the contract -----------------------------------------------------

    def validate(self) -> None:
        if not self.content:
            raise Invalid("a record must say what it contains")
        missing, ok = understands(self.critical)
        if not ok:
            raise UnknownSchema(f"requires {missing!r}")
        for name, value in (self.extensions or {}).items():
            if not str(name).strip():
                raise Invalid("an extension needs a name saying who understands it")
        if self.intent is not None and self.intent.want not in _WANTS:
            # Refused rather than treated as run. Guessing here means carrying
            # on with a job somebody asked to stop, using a word this
            # implementation is too old to understand.
            raise Invalid(f"intent {self.intent.want!r}")
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
        st = self.progress.step
        if st is not None:
            # A step with no name tells a person nothing, which is the only
            # thing a step is for.
            if not st.name.strip():
                raise Invalid("a step needs a name")
            if st.ordinal < 1:
                raise Invalid(f"step ordinal counts from one, got {st.ordinal}")
            if st.of > 0 and st.ordinal > st.of:
                raise Invalid(f"step {st.ordinal} of {st.of}")
            if st.done < 0 or st.total < 0:
                raise Invalid("step progress cannot be negative")
        if self.delegation is not None:
            if not self.delegation.system.strip() or not self.delegation.external_id.strip():
                raise Invalid("delegation needs both a system and an external id")

    def terminal(self) -> bool:
        return self.state in _TERMINAL

    def wants(self) -> str:
        """The desired state, which is RUN unless somebody said otherwise.

        Callers use this rather than testing ``intent`` for None, so that
        "nobody asked for anything" and "somebody asked for it to run" are
        the same answer everywhere — including for version 3 records, which
        have no intent at all.
        """
        if self.intent is None or not self.intent.want:
            return RUN
        return self.intent.want

    def paused(self) -> bool:
        """Somebody asked this to stop and it has not finished."""
        return self.wants() == PAUSE and not self.terminal()

    # ---- ranges -----------------------------------------------------------
    #
    # A checkpoint is opaque to this module, and these four methods do not make
    # it less so: they are a canonical form offered to a caller that has decided
    # its checkpoint holds proven ranges, not a meaning read out of every record
    # relayed. See the range section above.

    def checkpoint_ranges(self) -> List[Range]:
        """The proven ranges this record's checkpoint carries.

        A record that has never checkpointed, and one whose checkpoint predates
        ranges entirely, both answer without an error: the first with the empty
        set, the second with the prefix as one range.
        """
        return ranges_from_checkpoint(self.checkpoint)

    def set_checkpoint_ranges(self, ranges) -> None:
        """Record what is proven, merged into canonical form, and declare the
        ranges model.

        Both halves matter. Without the canonical form two writers spell one
        state two ways; without the declaration a reader cannot tell whether an
        absent ``verified`` means "nothing proven beyond the prefix" or "this
        writer had never heard of ranges".
        """
        self.checkpoint = checkpoint_with_ranges(self.checkpoint, ranges)
        if MODEL_RANGES not in self.content:
            self.content = list(self.content) + [MODEL_RANGES]

    def add_checkpoint_range(self, start: int, end: int) -> None:
        """Fold one newly proven range into the checkpoint. What a parallel
        fetcher calls as each part lands."""
        self.set_checkpoint_ranges(list(self.checkpoint_ranges()) + [(start, end)])

    def clear_checkpoint_ranges(self) -> None:
        """Remove the ranges and the declaration, leaving every other key alone.

        The declaration is carried rather than derived, so it has to be
        withdrawn explicitly; a record that kept declaring a model whose data it
        no longer holds sends a reader looking for something that is not there.
        """
        if isinstance(self.checkpoint, dict):
            rest = {k: v for k, v in self.checkpoint.items() if k != VERIFIED_KEY}
            self.checkpoint = {k: rest[k] for k in sorted(rest)} or None
        self.content = [n for n in self.content if n != MODEL_RANGES]

    def delegated(self) -> bool:
        """Something outside this process is doing the work.

        A caller finding this true must not start working itself: the external
        system does not participate in the lease, so two workers is exactly the
        damage the lease exists to prevent.
        """
        return self.delegation is not None

    def to_json(self) -> bytes:
        # Describe first, then check, which is the order Go's Encode and C++'s
        # encode() use. This module used to validate first, so a record whose
        # declaration had not been filled in yet was refused for saying nothing
        # about itself -- while the other two implementations derived the
        # declaration and wrote it. Each half was self-consistent, so no unit
        # test in one language could see it.
        #
        # What this implementation writes, it writes as its own version. A
        # record read at 3 and written back at 3 while carrying an intent would
        # tell an older reader it is safe to ignore fields it does not know,
        # which is exactly the risk the schema check refuses.
        self.describe()
        self.validate()
        # Key order and omissions match the Go struct tags exactly. Go decodes
        # with DisallowUnknownFields, so an extra key here is not a cosmetic
        # difference — it makes the record unreadable to the other half of the
        # abstraction.
        d: Dict[str, Any] = {
            "content": list(self.content),
        }
        if self.critical:
            d["critical"] = list(self.critical)
        d.update({
            "id": self.id,
            "kind": self.kind,
            "state": self.state,
            "spec": self.spec,
        })
        if self.checkpoint is not None:
            d["checkpoint"] = self.checkpoint
        d["progress"] = {"done": self.progress.done}
        if self.progress.total:
            d["progress"]["total"] = self.progress.total
        d["progress"]["updated_at"] = _rfc3339(self.progress.updated_at)
        if self.progress.step is not None:
            st = {"name": self.progress.step.name, "ordinal": self.progress.step.ordinal}
            if self.progress.step.of:
                st["of"] = self.progress.step.of
            if self.progress.step.done:
                st["done"] = self.progress.step.done
            if self.progress.step.total:
                st["total"] = self.progress.step.total
            d["progress"]["step"] = st
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
        if self.intent is not None:
            it: Dict[str, Any] = {"want": self.intent.want}
            if self.intent.by:
                it["by"] = self.intent.by
            if self.intent.at is not None:
                it["at"] = _rfc3339(self.intent.at)
            d["intent"] = it
        if self.extensions:
            # Sorted, because a dict has no order the other implementations
            # share and this output is compared byte for byte against them.
            d["extensions"] = {k: self.extensions[k] for k in sorted(self.extensions)}
        d["created_at"] = _rfc3339(self.created_at)
        d["updated_at"] = _rfc3339(self.updated_at)
        return (json.dumps(d, indent=2) + "\n").encode("utf-8")

    def describe(self) -> None:
        """Fill in content and critical from what this record actually carries.

        Derived rather than remembered, so the declaration cannot drift from the
        data: a record that gained an intent since it was last written says so on
        the next write without anybody updating a list.

        Caller-declared criticals are preserved -- an extension's writer may have
        said "refuse this record if you cannot read my payload", and this layer
        is not entitled to downgrade that.
        """
        content = [MODEL_BASE]
        critical = [MODEL_BASE]
        if self.intent is not None:
            content.append(MODEL_INTENT)
            critical.append(MODEL_INTENT)
        if self.delegation is not None:
            content.append(MODEL_DELEGATION)
            critical.append(MODEL_DELEGATION)
        # Advisory, and deliberately not critical: a reader that ignores a step
        # is correct about everything that matters.
        if self.progress.step is not None:
            content.append(MODEL_STEP)
        # A model this layer declares but cannot derive, because what it
        # describes lives inside the checkpoint and a checkpoint is opaque here.
        # Rediscovering it would mean reading one, so the declaration is carried
        # instead -- the same treatment an unknown extension gets. Sorted, and
        # placed here rather than among the extension names, so all three
        # implementations put it in the same slot. See CARRIED_MODELS.
        content.extend(sorted({n for n in self.content or () if n in CARRIED_MODELS}))
        content.extend(sorted(self.extensions or {}))

        for name in self.critical or ():
            # Except the models whose own definition says a reader ignoring them
            # is still correct. Marking one critical tells a stranger to refuse
            # the job over a decoration, and this layer will not relay that
            # however it arrived.
            if name in NEVER_CRITICAL:
                continue
            if name not in critical and name in content:
                critical.append(name)

        self.content = content
        self.critical = critical

    @staticmethod
    def from_json(b: bytes) -> "Record":
        try:
            d = json.loads(b)
        except ValueError as e:
            raise Invalid(str(e)) from None
        content = list(d.get("content") or ())
        critical = list(d.get("critical") or ())
        if not content and "schema" in d:
            # A legacy record. The mapping is exact, not a guess: those versions
            # are frozen and it is known what each one could contain.
            models = LEGACY_SCHEMAS.get(d.get("schema"))
            if models is None:
                raise UnknownSchema(f"legacy schema {d.get('schema')}")
            content = list(models)
            critical = [m for m in models if m != MODEL_STEP]
            if d.get("delegation") is not None:
                content.append(MODEL_DELEGATION)
                critical.append(MODEL_DELEGATION)
        missing, ok = understands(critical)
        if not ok:
            raise UnknownSchema(
                f"this record requires {missing!r}, which this implementation cannot read"
            )
        p = d.get("progress") or {}
        l = d.get("lease") or {}
        dg = d.get("delegation")
        it = d.get("intent")
        r = Record(
            id=d.get("id", ""),
            kind=d.get("kind", ""),
            state=d.get("state", ""),
            content=content,
            critical=critical,
            extensions=dict(d.get("extensions") or {}),
            spec=d.get("spec"),
            checkpoint=d.get("checkpoint"),
            progress=Progress(
                step=(
                    Step(
                        name=(p.get("step") or {}).get("name", ""),
                        ordinal=(p.get("step") or {}).get("ordinal", 0),
                        of=(p.get("step") or {}).get("of", 0),
                        done=(p.get("step") or {}).get("done", 0),
                        total=(p.get("step") or {}).get("total", 0),
                    )
                    if isinstance(p.get("step"), dict)
                    else None
                ),
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
            intent=(
                Intent(
                    want=it.get("want", RUN),
                    by=it.get("by", ""),
                    at=_parse_time(it["at"]) if it.get("at") else None,
                )
                if it
                else None
            ),
            created_at=_parse_time(d.get("created_at", "")),
            updated_at=_parse_time(d.get("updated_at", "")),
        )
        r.validate()
        return r


def new_id() -> str:
    """An id that sorts by creation time, so a directory listing is in roughly
    submission order without anything having to record that separately."""
    return f"{int(time.time() * 1000)}-{binascii.hexlify(secrets.token_bytes(10)).decode()}"


@runtime_checkable
class Store(Protocol):
    """Where jobs live. Every other abstraction in this project sits on it.

    # What this protocol is, and what it deliberately is not

    It is the SEMANTICS of the lease protocol and nothing else: a claim is
    exclusive, an epoch only ever increases, every write must present the epoch
    it holds, and a successor may continue only from what a predecessor proved.
    That is what two implementations have to agree about, and not one clause of
    it mentions a byte.

    It is NOT a file format and NOT a transport. Records have to be represented
    somehow and a call has to reach whoever executes it somehow, but both belong
    to the BINDING underneath -- files in a directory today, a service over a
    socket next, a machine across the network after that. Changing the
    representation must be invisible from here.

    Go and C++ both had this and Python did not. The whole public surface of
    this module was ``FileStore``, so the only type Python offered named its own
    binding -- and it showed one layer up, where the download runner reached
    through ``store.root`` for a directory that a service binding does not have.

    The test an abstraction has to pass is THE SAME APPLICATION, UNCHANGED,
    RUNNING ON TWO BINDINGS.
    """

    def submit(self, r: "Record") -> str:
        """Record new work and return its id. The id is the handle: a plain
        string that outlives the process which created it."""
        ...

    def load(self, job_id: str) -> "Record":
        """Read a record. Any process may, including one that holds no lease and
        never will -- that is what makes work observable from outside, which a
        callback cannot be, because a callback is bound to the lifetime of the
        process that registered it and that is exactly the lifetime which
        fails."""
        ...

    def list(self) -> List["Record"]:
        """Every job, oldest first."""
        ...

    def claimable(self, r: "Record") -> bool:
        """Whether this job can be taken over right now."""
        ...

    def orphans(self) -> List["Record"]:
        """Work nobody is doing. The reclamation path, and primary rather than a
        fallback: a process that is killed never hands anything over, so a
        design relying on graceful handoff has no answer for the case that loses
        a 40 GB download."""
        ...

    def claim(self, job_id: str, owner: str, ttl_seconds: float) -> "Record":
        """Take ownership for ttl_seconds and return the record carrying the
        caller's new epoch. Exclusive: two callers cannot hold the same epoch."""
        ...

    def renew(self, job_id: str, epoch: int, ttl_seconds: float) -> "Record":
        """Extend a lease the caller still holds. Must refuse once the lease has
        expired even when the epoch still matches, because a process suspended
        for an hour wakes up believing it is still the owner."""
        ...

    def release(self, job_id: str, epoch: int) -> None:
        """Give up a lease early. A courtesy: everything works without it, only
        more slowly."""
        ...

    def update(self, job_id: str, epoch: int, mutate: Callable[["Record"], None]) -> "Record":
        """Apply mutate, but only if the caller still holds the lease at the
        epoch it presents. The single gate every change passes through."""
        ...

    def set_intent(self, job_id: str, want: str, by: str = "") -> "Record":
        """Say what should happen, WITHOUT a lease.

        The one write that presents no epoch. Whoever wants a job stopped is not
        the process doing it -- a person clicks cancel while a service on another
        machine moves the bytes -- and requiring a lease would mean stealing the
        job in order to stop it, which is what the lease prevents.
        """
        ...


@runtime_checkable
class Scratch(Protocol):
    """An OPTIONAL capability: a store whose binding happens to BE a local
    filesystem can offer an area on it.

    It exists to contain a leak, not to bless one. ``store.root`` was an
    attribute anyone could reach for, and the download runner did, to resolve a
    relative sink. A store bound to a service has no directory to give, so the
    caller must ask::

        if isinstance(store, Scratch):
            ...

    and have a real answer for no, rather than assuming a directory exists.
    """

    def root(self) -> str:
        """The local area this binding keeps its own state in. Relative paths in
        a record resolve against it, so a record written by a PC and read by a
        NAS names one directory rather than one machine's view of it."""
        ...

    def work_path(self, job_id: str) -> str:
        """Scratch space for a single job. What a kind puts there is its own
        business; the only guarantee is that the location is derived from the id,
        so a successor can find what a predecessor left."""
        ...


# How long a claim token may exist without its record having caught up before
# the claimant that made it is presumed gone. The two writes are microseconds
# apart in the same function, so anything on this scale is a crash rather than a
# slow disk -- and generous even for a store on a share.
_CLAIM_HANDOVER_SECONDS = 10.0

# A bound on how far a claim will step over abandoned tokens before giving up
# and saying so, rather than looping.
_MAX_EPOCH_SKIP = 64


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
        self._root = root
        self._now = now or (lambda: datetime.now(timezone.utc))
        os.makedirs(os.path.join(root, "jobs"), exist_ok=True)
        os.makedirs(os.path.join(root, "work"), exist_ok=True)

    # ---- paths ------------------------------------------------------------

    def root(self) -> str:
        """The local area this binding keeps its own state in.

        A method and not a field on purpose. As an attribute it was reachable
        from anywhere without anyone having to admit they needed a directory,
        and the download runner duly reached for it -- so the layer that is not
        supposed to know what a file is could not run on a store that has no
        files. Now it is the Scratch capability, and a caller that wants it must
        ask whether this binding has one.
        """
        return self._root

    def _record_path(self, job_id: str) -> str:
        return os.path.join(self._root, "jobs", job_id + ".json")

    def _epoch_path(self, job_id: str, epoch: int) -> str:
        return os.path.join(self._root, "jobs", f"{job_id}.epoch.{epoch}")

    def work_path(self, job_id: str) -> str:
        """Scratch space a job may use while it runs. What goes there is the
        kind's business; the store only guarantees the path is derived from the
        id, so a successor can find what a predecessor left."""
        return os.path.join(self._root, "work", job_id)

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
        d = os.path.join(self._root, "jobs")
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

        A TRANSFERRED job is not an orphan, and the difference cost a NAS 313 MB.
        Transferred is not terminal — deliberately, because the requester still
        has to take delivery — so ``claimable`` says yes to it, and a supervisor
        sweeping for stranded work saw a finished, digest-proven job with a
        lapsed lease and downloaded the whole thing again. Then again 30 seconds
        later, forever. What a transferred job waits for is an acknowledgement,
        and no amount of re-downloading produces one.

        A PAUSED job is not an orphan either, and for the same reason: it looks
        abandoned and is not. Somebody asked it to stop, so the lease was
        released deliberately — and a sweep that adopted it would start the work
        again seconds after a person pressed pause, which is worse than never
        having offered pause at all.
        """
        return [
            r for r in self.list()
            if self.claimable(r) and r.state != TRANSFERRED and not r.paused()
        ]

    # ---- writing ----------------------------------------------------------

    def set_intent(self, job_id: str, want: str, by: str = "") -> Record:
        """Say what should happen, WITHOUT holding the lease.

        The only write here that presents no epoch, and deliberately so: the
        party who wants a job stopped is not the process doing it, and requiring
        a lease would mean stealing the job in order to stop it — the one thing
        the lease exists to prevent.

        Idempotent, and refused once the job is terminal, because nothing
        reopens finished work. Asking for something the current owner cannot do
        is NOT an error here: only the owner knows what it can do, so only the
        owner can say so.
        """
        if want not in _WANTS:
            raise Invalid(f"intent {want!r}")
        r = self.load(job_id)
        if r.terminal():
            raise Terminal(f"{job_id} is {r.state}")
        now = self._now()
        r.intent = Intent(want=want, by=by, at=now)
        r.updated_at = now
        self._write(r)
        return r

    def submit(self, r: Record) -> str:
        if not r.id:
            r.id = new_id()
        now = self._now()
        r.describe()
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

        fd, nxt = self._take_epoch(job_id, r.lease.epoch + 1)
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

        # The previous epoch's token can go now. Nobody will ever ask for it
        # again: a claimant derives its epoch from the RECORD, which now says
        # nxt. Left alone these accumulate one file per claim, forever -- a real
        # store reached 1069 files for 17 jobs.
        if nxt > 1:
            try:
                os.remove(self._epoch_path(job_id, nxt - 1))
            except OSError:
                pass
        return r

    def _age_on_store(self, mtime: float) -> float:
        """How old a file is according to the clock that stamped it.

        The obvious ``time.time() - mtime`` compares two clocks: this machine's,
        and whatever holds the store. On a share those are different machines. A
        store host running behind by more than the handover makes every freshly
        written token look abandoned, so two claimants skip past each other and
        both start work -- which is what the epoch exists to prevent.

        So the age is measured against a mark the store itself just made. When
        that cannot be done the answer is "too recent to touch": refusing a claim
        costs a retry, taking one wrongly costs correctness.
        """
        jobs = os.path.join(self._root, "jobs")
        try:
            fd, probe = tempfile.mkstemp(prefix=".now-", dir=jobs)
            os.close(fd)
        except OSError:
            return 0.0
        try:
            return os.stat(probe).st_mtime - mtime
        except OSError:
            return 0.0
        finally:
            try:
                os.unlink(probe)
            except OSError:
                pass

    def _take_epoch(self, job_id: str, first: int):
        """Create the claim token for the first epoch at or after `first` that
        nobody holds, and return (fd, epoch).

        # Why this is not simply "create the next one"

        It was, and a job could be bricked by it. The token is created BEFORE the
        record is written, so a process that dies in between leaves a token for
        an epoch the record never reached. Every later claim then computes the
        same next epoch, finds that token, and fails -- permanently. The job
        cannot be claimed, so it cannot be updated, cancelled, adopted or
        finished by anyone, ever.

        Seen on a live store: record at epoch 216, a token for 217, and a
        supervisor reporting a healthy sweep every five seconds while that job
        silently failed its claim. Setting an intent on it did nothing either,
        because honouring an intent requires claiming first.

        # Why skipping is safe, and when it is not

        Skipping past a HELD epoch would destroy the exclusivity the token exists
        to provide, so freshness decides. A claimant that is genuinely mid-flight
        wrote its token moments ago and is about to write the record; this claim
        must lose to it. A token that has sat there while the record stayed
        behind belongs to nobody.

        Compared against real time, not the store's clock: an injected test clock
        says nothing about when the filesystem wrote a file.
        """
        for nxt in range(first, first + _MAX_EPOCH_SKIP):
            path = self._epoch_path(job_id, nxt)
            try:
                return os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644), nxt
            except FileExistsError:
                try:
                    age = self._age_on_store(os.stat(path).st_mtime)
                except OSError:
                    raise LeaseHeld(f"epoch {nxt} was taken by someone else") from None
                if age < _CLAIM_HANDOVER_SECONDS:
                    raise LeaseHeld(f"epoch {nxt} was taken by someone else") from None
                # Abandoned. Take the epoch after it.
        raise LeaseHeld(
            f"{_MAX_EPOCH_SKIP} epochs from {first} are all spoken for"
        )

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
            # A delegated job stays RUNNING. Letting go of the lease means "I am
            # not the one watching this any more", not "this stopped" — the work
            # is going on inside BITS, or on a NAS, and demoting it to pending
            # tells every supervisor sweeping for stranded work to start it over
            # somewhere else. Delegating releases the lease immediately, so this
            # is not an edge case; it is what delegation does.
            if r.state == RUNNING and not r.delegated():
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
        d = os.path.join(self._root, "jobs")
        tmp = os.path.join(d, f"{r.id}.tmp-{os.getpid()}-{secrets.token_hex(4)}")
        with open(tmp, "wb") as f:
            f.write(data)
        os.replace(tmp, self._record_path(r.id))


class Memory:
    """Jobs in a dict, for a process that wants the semantics and no disk.

    The point of this binding is not convenience. A socket in front of a
    FileStore is a transport swap and proves little; this shares no code with
    FileStore at all. The lease, the epoch, the exclusivity, the refusal of a
    stale write, and the rule that a paused or transferred job is not an orphan
    are all written again here. If those semantics were really FileStore's
    filesystem tricks all along -- exclusive create, atomic rename -- this is
    where that shows, because a dict has neither.

    Deliberately NOT a Scratch: there is no directory, and a caller that assumed
    one gets told so instead of being handed a path that does not exist. That is
    the whole reason the capability is separate.

    Not durable and not shared between processes, which is exactly what makes it
    honest about being one binding among several rather than the definition.
    """

    def __init__(self, now: Callable[[], datetime] = None):
        self._jobs: Dict[str, bytes] = {}
        self._now = now or (lambda: datetime.now(timezone.utc))

    # Stored encoded, and decoded on every read. Not a detail: handing out the
    # same object twice would let a caller mutate a record the store still
    # believes it owns, so an in-memory store would be the one binding where a
    # write did not have to pass the epoch gate. Encoding also means this
    # binding is held to the same record rules as any other -- a Record that
    # would not survive a round trip does not survive here either.
    def _get(self, job_id: str) -> Record:
        raw = self._jobs.get(job_id)
        if raw is None:
            raise NotFound(job_id)
        return Record.from_json(raw)

    def _put(self, r: Record) -> None:
        self._jobs[r.id] = r.to_json()

    # ---- reading ----------------------------------------------------------

    def load(self, job_id: str) -> Record:
        return self._get(job_id)

    def list(self) -> List[Record]:
        return [Record.from_json(raw) for _, raw in sorted(self._jobs.items())]

    def claimable(self, r: Record) -> bool:
        return not r.terminal() and not r.lease.held(self._now())

    def orphans(self) -> List[Record]:
        return [
            r for r in self.list()
            if self.claimable(r) and r.state != TRANSFERRED and not r.paused()
        ]

    # ---- writing ----------------------------------------------------------

    def submit(self, r: Record) -> str:
        if not r.id:
            r.id = new_id()
        if r.id in self._jobs:
            raise Invalid(f"{r.id} already exists")
        now = self._now()
        r.describe()
        if not r.state:
            r.state = PENDING
        r.created_at = now
        r.updated_at = now
        r.progress.updated_at = now
        self._put(r)
        return r.id

    def claim(self, job_id: str, owner: str, ttl_seconds: float) -> Record:
        if not owner.strip():
            raise JobError("claim requires an owner")
        r = self._get(job_id)
        if r.terminal():
            raise Terminal(f"{job_id} is {r.state}")
        now = self._now()
        if r.lease.held(now) and r.lease.owner != owner:
            raise LeaseHeld(f"{r.lease.owner} holds it until {_rfc3339(r.lease.expires_at)}")
        r.lease = Lease(
            owner=owner,
            epoch=r.lease.epoch + 1,
            expires_at=datetime.fromtimestamp(now.timestamp() + ttl_seconds, timezone.utc),
        )
        if r.state in (PENDING, RUNNING):
            r.state = RUNNING
        self._put(r)
        return r

    def renew(self, job_id: str, epoch: int, ttl_seconds: float) -> Record:
        r = self._get(job_id)
        if r.lease.epoch != epoch:
            raise StaleEpoch(f"record is at epoch {r.lease.epoch}, caller holds {epoch}")
        if not r.lease.held(self._now()):
            raise LeaseExpired(f"expired at {_rfc3339(r.lease.expires_at)}, re-claim instead")
        now = self._now()
        r.lease.expires_at = datetime.fromtimestamp(now.timestamp() + ttl_seconds, timezone.utc)
        self._put(r)
        return r

    def release(self, job_id: str, epoch: int) -> None:
        def mutate(r: Record) -> None:
            r.lease.expires_at = self._now()
            r.lease.owner = ""
            # A delegated job stays RUNNING: letting go of the lease means "I am
            # not the one watching this any more", not "this stopped".
            if r.state == RUNNING and not r.delegated():
                r.state = PENDING

        self.update(job_id, epoch, mutate)

    def update(self, job_id: str, epoch: int, mutate: Callable[[Record], None]) -> Record:
        r = self._get(job_id)
        if r.lease.epoch != epoch:
            raise StaleEpoch(f"record is at epoch {r.lease.epoch}, caller holds {epoch}")
        if not r.lease.held(self._now()):
            raise LeaseExpired(f"expired at {_rfc3339(r.lease.expires_at)}")
        mutate(r)
        r.updated_at = self._now()
        self._put(r)
        return r

    def set_intent(self, job_id: str, want: str, by: str = "") -> Record:
        if want not in _WANTS:
            raise Invalid(f"intent {want!r}")
        r = self._get(job_id)
        if r.terminal():
            raise Terminal(f"{job_id} is {r.state}")
        now = self._now()
        r.intent = Intent(want=want, by=by, at=now)
        r.updated_at = now
        self._put(r)
        return r
