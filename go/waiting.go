package job

import "strings"

// heldBy begins the card word abstraction-resource registers in the
// catalogue and owns as service.WaitingHeldBy: an unfinished job waits for
// the program named after it to release the graphics card (JOB-A15,
// RES-L8). testdata/waiting-sentences.json ties the two together.
const heldBy = "card:held_by:"

// WaitingSentence is the sentence a client prints when it tells a person why
// a job waits, for a waiting word from job's catalogue (JOB-A15). It is empty
// for an empty word, and an unrecognized word reads as an unnamed wait.
func WaitingSentence(word string) string {
	switch {
	case word == "":
		return ""
	case strings.HasPrefix(word, heldBy) && len(word) > len(heldBy):
		return "Waiting for the graphics card: held by " + word[len(heldBy):] + "."
	default:
		return "Waiting."
	}
}
