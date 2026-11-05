package main

import "strings"

// Audio track titles. Recordings store them as stream titles, and the editor
// uses them to find the mixed track and the parts it was made from.
const (
	TitleGame    = "Game audio"
	TitleDiscord = "Discord"
	TitleMic     = "Microphone"
)

// sourceTitles are the single-source tracks a mixed track can be rebuilt from.
var sourceTitles = map[string]string{TitleGame: "Game", TitleDiscord: "Discord", TitleMic: "Mic"}

// MixTitle names a mixed track after what's in it, e.g. "Game + Discord + Mic".
// Game + mic gives "Game + Mic", the same as clips from earlier versions.
func MixTitle(titles []string) string {
	var parts []string
	for _, t := range titles {
		if short, ok := sourceTitles[t]; ok {
			parts = append(parts, short)
		} else {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " + ")
}

// isMixTitle reports whether a track title is a ReKapture mixed track.
func isMixTitle(t string) bool { return strings.Contains(t, " + ") }
