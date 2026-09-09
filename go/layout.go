package job

import "strings"

// Which relative paths under a store root belong to the store itself.
//
// A layer above this one — download — lets a caller name where bytes land, and
// resolves a relative destination against the store root. Containment was added
// so such a path could not climb OUT of the root. It could still aim at the
// root's own contents: a final of `jobs/<id>.json` overwrites a job record, and
// a final of `work/<other>` overwrites another job's partial. Both are inside
// the root and both passed every containment check.
//
// The layout is this binding's, so the answer is this binding's to give. The
// alternative was download spelling "jobs" and "work" itself, which is exactly
// the coupling job.Record.Spec's opacity exists to prevent — the store could
// then never rename a directory without breaking a layer that is not supposed
// to know it has directories at all.

// Reserved reports whether rel — a path relative to the store root — names part
// of the file binding's own layout rather than free space inside it.
//
// owner is the job asking. Its own scratch, work/<owner> and everything under
// it, is not reserved against that job and is reserved against every other, so
// a download can still put its partial where the store told it to. An empty
// owner asks on behalf of no job, which reserves the whole of work/ — the right
// answer for a caller that has not been given an id yet.
//
// Case is folded and a trailing dot or space is trimmed from every segment, on
// every platform. That is deliberately unlike the containment comparison, which
// folds case only where the filesystem does: containment compares two paths on
// THIS machine, and this compares a record's path against names the contract
// fixed. A record refused by Windows and accepted by Linux would mean the
// refusal depends on who looked — and `Jobs/x.json` does land in `jobs/` on
// NTFS, as does `jobs./x.json`.
func Reserved(owner, rel string) bool {
	segs := storeSegments(rel)
	if len(segs) == 0 {
		return false
	}
	switch segs[0] {
	case "jobs":
		return true
	case "work":
		return len(segs) < 2 || segs[1] != foldSegment(owner)
	case registryFile:
		return len(segs) == 1
	}
	return false
}

// registryFile is the discovery registry, which sits at the root beside jobs/
// and work/ rather than inside either. See job/cpp/src/discovery_client.cpp and
// download/python/abstraction_discovery.py, which read it.
const registryFile = "services.json"

// RootName is the name rel takes in the store root, or "" if rel names
// something deeper, climbs out of the root, or is empty.
//
// The root is a shared namespace and this binding is not its only occupant: the
// download layer keeps a supervisor heartbeat and a nudge socket beside jobs/
// and work/. A layer that puts a file there has to be able to ask whether a
// path aims at it, and to ask in the same spelling — folded the same way, with
// the same separators — or the two of them protect different sets of names.
// See download.ReservedSink.
func RootName(rel string) string {
	segs := storeSegments(rel)
	if len(segs) != 1 {
		return ""
	}
	return segs[0]
}

// storeSegments reduces a relative path to the segments a store would see, in
// the one spelling two of them can be compared in.
//
// A path that climbs above the root returns nothing: that is containment's
// question, already answered before this one is asked, and answering it again
// here with a different rule would give two verdicts on one record.
func storeSegments(rel string) []string {
	var out []string
	for _, seg := range strings.FieldsFunc(rel, isSeparator) {
		switch seg {
		case ".":
		case "..":
			if len(out) == 0 {
				return nil
			}
			out = out[:len(out)-1]
		default:
			out = append(out, foldSegment(seg))
		}
	}
	return out
}

func isSeparator(r rune) bool { return r == '/' || r == '\\' }

// foldSegment is one segment in the spelling a filesystem would give it.
//
// Windows drops a trailing dot or space from a name, so `jobs.` opens `jobs`.
// Dropping it here too keeps the answer the same on both platforms, which is
// the property that matters. A name that is NOTHING but dots and spaces is left
// alone — it is a real name on POSIX, and not one Windows will open at all.
func foldSegment(s string) string {
	trimmed := strings.TrimSpace(s)
	if cut := strings.TrimRight(trimmed, ". "); cut != "" {
		trimmed = cut
	}
	return strings.ToLower(trimmed)
}
